// Package coverage measures raw and conservatively classified Go statement coverage.
// It never executes source code or uses hit counts to choose exclusions.
package coverage

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const AnalyzerVersion = "go-functional-v2"

type Policy struct {
	SchemaVersion           string `json:"schema_version"`
	ExcludePureErrorReturns bool   `json:"exclude_pure_error_returns"`
	Text                    string `json:"text"`
}

func DefaultPolicy() Policy {
	return Policy{"v1", false, "Retain all statements unless the AgentClass policy explicitly excludes them."}
}

type Exclusion struct {
	File         string `json:"file"`
	SourceDigest string `json:"source_digest"`
	StartLine    int    `json:"start_line"`
	StartColumn  int    `json:"start_column"`
	EndLine      int    `json:"end_line"`
	EndColumn    int    `json:"end_column"`
	Statements   int64  `json:"statements"`
	Reason       string `json:"reason"`
}
type Manifest struct {
	SchemaVersion string      `json:"schema_version"`
	Exclusions    []Exclusion `json:"exclusions"`
}
type Metric struct {
	DisplayPercent string   `json:"display_percent"`
	Covered        int64    `json:"covered"`
	Total          int64    `json:"total"`
	Percent        *float64 `json:"percent"`
}
type Report struct {
	// Blocks is coverage-blind source evidence for applying a caller-defined policy.
	// It is exposed separately by describe-blocks, never mixed into metric output.
	Blocks          []Exclusion `json:"-"`
	Comparison      *Comparison `json:"comparison,omitempty"`
	SchemaVersion   string      `json:"schema_version"`
	AnalyzerVersion string      `json:"analyzer_version"`
	PolicyHash      string      `json:"policy_hash"`
	Policy          Policy      `json:"policy"`
	Raw             Metric      `json:"raw"`
	Functional      Metric      `json:"functional"`
	Excluded        Metric      `json:"excluded"`
	Exclusions      []Exclusion `json:"exclusions"`
	ExclusionsHash  string      `json:"exclusions_hash"`
}

func decode(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}
func ReadReport(r io.Reader) (Report, error) { var out Report; err := decode(r, &out); return out, err }
func ReadPolicy(r io.Reader) (Policy, error) {
	var p Policy
	err := decode(r, &p)
	if err == nil && p.SchemaVersion != "v1" {
		err = fmt.Errorf("unsupported policy schema %q", p.SchemaVersion)
	}
	return p, err
}
func ReadManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	err := decode(r, &m)
	if err == nil && m.SchemaVersion != "v1" {
		err = fmt.Errorf("unsupported exclusion schema %q", m.SchemaVersion)
	}
	return m, err
}
func digest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
func metric(covered, total int64) Metric {
	m := Metric{Covered: covered, Total: total, DisplayPercent: "unavailable"}
	if total > 0 {
		v := 100 * float64(covered) / float64(total)
		m.Percent = &v
		m.DisplayPercent = fmt.Sprintf("%.2f%%", v)
	}
	return m
}

type block struct {
	Exclusion
	hit bool
}
type source struct {
	data  []byte
	lines []string
	pure  map[string]bool
}

func coords(sl, sc, el, ec int) string { return fmt.Sprintf("%d.%d,%d.%d", sl, sc, el, ec) }
func (e Exclusion) key() string {
	return e.File + ":" + coords(e.StartLine, e.StartColumn, e.EndLine, e.EndColumn)
}

var profileLine = regexp.MustCompile(`^(.+):([0-9]+)\.([0-9]+),([0-9]+)\.([0-9]+) ([0-9]+) ([0-9]+)$`)

// Analyze validates a possibly concatenated set of Go profiles and unions
// identical blocks. The source directory must be the exact instrumented checkout.
// A profile carries no source digest; callers must authenticate that checkout.
func Analyze(r io.Reader, dir string, p Policy, manifest Manifest) (Report, error) {
	out := Report{SchemaVersion: "v1", AnalyzerVersion: AnalyzerVersion, Policy: p, Exclusions: []Exclusion{}}
	if p.SchemaVersion != "v1" {
		return out, fmt.Errorf("unsupported policy schema %q", p.SchemaVersion)
	}
	if manifest.SchemaVersion != "" && manifest.SchemaVersion != "v1" {
		return out, fmt.Errorf("unsupported exclusion schema %q", manifest.SchemaVersion)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return out, fmt.Errorf("read source module: %w", err)
	}
	module := ""
	for _, line := range strings.Split(string(mod), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "module" {
			module = strings.Trim(f[1], `"`)
			break
		}
	}
	if module == "" {
		return out, fmt.Errorf("source go.mod has no module directive")
	}
	absRoot, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return out, err
	}
	absRoot, err = filepath.Abs(absRoot)
	if err != nil {
		return out, err
	}
	sources := map[string]*source{}
	blocks := map[string]*block{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	mode := ""
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.HasPrefix(line, "mode: ") {
			v := strings.TrimPrefix(line, "mode: ")
			if v != "set" && v != "count" && v != "atomic" {
				return out, fmt.Errorf("profile line %d: invalid mode", lineNo)
			}
			if mode != "" && mode != v {
				return out, fmt.Errorf("conflicting coverage modes")
			}
			mode = v
			continue
		}
		if mode == "" {
			return out, fmt.Errorf("missing coverage mode")
		}
		parts := profileLine.FindStringSubmatch(line)
		if parts == nil {
			return out, fmt.Errorf("profile line %d: malformed block", lineNo)
		}
		name := strings.TrimPrefix(parts[1], module+"/")
		if filepath.IsAbs(name) || strings.Contains(name, "\\") || name != filepath.ToSlash(filepath.Clean(name)) || name == ".." || strings.HasPrefix(name, "../") || !strings.HasSuffix(name, ".go") {
			return out, fmt.Errorf("unsafe or unsupported profile path %q", parts[1])
		}
		nums := make([]int64, 6)
		for i := range nums {
			nums[i], err = strconv.ParseInt(parts[i+2], 10, 64)
			if err != nil {
				return out, fmt.Errorf("profile line %d: %w", lineNo, err)
			}
		}
		for _, n := range nums[:4] {
			if n <= 0 || n > int64(math.MaxInt) {
				return out, fmt.Errorf("invalid block coordinates/count")
			}
		}
		if mode == "set" && nums[5] > 1 {
			return out, fmt.Errorf("set profile hit count must be zero or one")
		}
		b := &block{Exclusion: Exclusion{File: name, StartLine: int(nums[0]), StartColumn: int(nums[1]), EndLine: int(nums[2]), EndColumn: int(nums[3]), Statements: nums[4]}, hit: nums[5] > 0}
		src := sources[name]
		if src == nil {
			target, evalErr := filepath.EvalSymlinks(filepath.Join(absRoot, filepath.FromSlash(name)))
			if evalErr != nil {
				return out, fmt.Errorf("source %q: %w", name, evalErr)
			}
			rel, relErr := filepath.Rel(absRoot, target)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return out, fmt.Errorf("source path escapes checkout: %q", name)
			}
			data, readErr := os.ReadFile(target)
			if readErr != nil {
				return out, readErr
			}
			src, err = parseSource(name, data)
			if err != nil {
				return out, err
			}
			sources[name] = src
		}
		if !validPosition(src, b.StartLine, b.StartColumn) || !validPosition(src, b.EndLine, b.EndColumn) || b.StartLine > b.EndLine || (b.StartLine == b.EndLine && b.StartColumn >= b.EndColumn) {
			return out, fmt.Errorf("block coordinates do not map to source: %s", b.key())
		}
		b.SourceDigest = digest(src.data)
		if old := blocks[b.key()]; old != nil {
			if old.Statements != b.Statements {
				return out, fmt.Errorf("conflicting statement counts for %s", b.key())
			}
			old.hit = old.hit || b.hit
		} else {
			blocks[b.key()] = b
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	if mode == "" {
		return out, fmt.Errorf("missing coverage mode")
	}
	if err := validateExtents(blocks); err != nil {
		return out, err
	}
	explicit := map[string]Exclusion{}
	for _, e := range manifest.Exclusions {
		b := blocks[e.key()]
		if b == nil {
			return out, fmt.Errorf("exclusion block absent from profile: %s", e.key())
		}
		if e.Statements <= 0 {
			return out, fmt.Errorf("explicit exclusions require a nonempty whole block: %s", e.key())
		}
		if strings.TrimSpace(e.Reason) == "" || e.SourceDigest != b.SourceDigest || e.Statements != b.Statements {
			return out, fmt.Errorf("exclusion reason, digest or count mismatch: %s", e.key())
		}
		if _, exists := explicit[e.key()]; exists {
			return out, fmt.Errorf("duplicate exclusion: %s", e.key())
		}
		explicit[e.key()] = e
	}
	keys := make([]string, 0, len(blocks))
	for k := range blocks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var covered, total, excludedCovered, excludedTotal int64
	for _, k := range keys {
		b := blocks[k]
		out.Blocks = append(out.Blocks, b.Exclusion)
		if b.Statements > math.MaxInt64-total {
			return out, fmt.Errorf("statement count overflow")
		}
		total += b.Statements
		if b.hit {
			covered += b.Statements
		}
		e, exclude := explicit[k]
		if !exclude && p.ExcludePureErrorReturns && b.Statements == 1 && sources[b.File].pure[coords(b.StartLine, b.StartColumn, b.EndLine, b.EndColumn)] {
			e = b.Exclusion
			e.Reason = "builtin:pure-error-propagation"
			exclude = true
		}
		if exclude {
			out.Exclusions = append(out.Exclusions, e)
			excludedTotal += b.Statements
			if b.hit {
				excludedCovered += b.Statements
			}
		}
	}
	// The policy fingerprint binds rules and analyzer version; source-dependent
	// classifications receive a separate fingerprint sorted by exact block.
	entries := append([]Exclusion{}, manifest.Exclusions...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
	policyJSON, err := json.Marshal(struct {
		Version string `json:"analyzer_version"`
		Policy  Policy `json:"policy"`
	}{AnalyzerVersion, p})
	if err != nil {
		return out, err
	}
	out.PolicyHash = digest(policyJSON)
	manifestJSON, err := json.Marshal(entries)
	if err != nil {
		return out, err
	}
	out.ExclusionsHash = digest(manifestJSON)
	out.Raw = metric(covered, total)
	out.Excluded = metric(excludedCovered, excludedTotal)
	out.Functional = metric(covered-excludedCovered, total-excludedTotal)
	return out, nil
}
func validPosition(s *source, line, col int) bool {
	return line >= 1 && line <= len(s.lines) && col >= 1 && col <= len(s.lines[line-1])+1
}
func parseSource(name string, data []byte) (*source, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, data, 0)
	if err != nil {
		return nil, fmt.Errorf("parse source %s: %w", name, err)
	}
	s := &source{data: data, lines: strings.Split(string(data), "\n"), pure: map[string]bool{}}
	ast.Inspect(f, func(n ast.Node) bool {
		guard, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		cond, ok := guard.Cond.(*ast.BinaryExpr)
		if !ok || cond.Op != token.NEQ {
			return true
		}
		id, ok := cond.X.(*ast.Ident)
		if !ok || id.Name != "err" {
			return true
		}
		nilID, ok := cond.Y.(*ast.Ident)
		if !ok || nilID.Name != "nil" {
			return true
		}
		if len(guard.Body.List) != 1 {
			return true
		}
		ret, ok := guard.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		last, ok := ret.Results[len(ret.Results)-1].(*ast.Ident)
		if !ok || last.Name != id.Name {
			return true
		}
		for _, expr := range ret.Results[:len(ret.Results)-1] {
			if !zeroValue(expr) {
				return true
			}
		}
		a, z := fset.Position(guard.Body.Lbrace), fset.Position(guard.Body.Rbrace+1)
		s.pure[coords(a.Line, a.Column, z.Line, z.Column)] = true
		return true
	})
	return s, nil
}
func zeroValue(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name == "nil" || v.Name == "false"
	case *ast.BasicLit:
		if v.Kind == token.INT {
			n, err := strconv.ParseInt(v.Value, 0, 64)
			return err == nil && n == 0
		}
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			return err == nil && s == ""
		}
		if v.Kind == token.FLOAT {
			n, err := strconv.ParseFloat(v.Value, 64)
			return err == nil && n == 0
		}
	}
	return false
}

// Distinct basic blocks cannot begin at the same point or cross one another.
// Containment is valid for instrumented function literals within outer blocks.
func validateExtents(blocks map[string]*block) error {
	grouped := map[string][]*block{}
	for _, b := range blocks {
		grouped[b.File] = append(grouped[b.File], b)
	}
	less := func(al, ac, bl, bc int) bool { return al < bl || (al == bl && ac < bc) }
	files := make([]string, 0, len(grouped))
	for name := range grouped {
		files = append(files, name)
	}
	sort.Strings(files)
	for _, name := range files {
		items := grouped[name]
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i], items[j]
			return less(a.StartLine, a.StartColumn, b.StartLine, b.StartColumn)
		})
		var stack []*block
		for i, b := range items {
			if i > 0 && items[i-1].StartLine == b.StartLine && items[i-1].StartColumn == b.StartColumn {
				return fmt.Errorf("conflicting block extents in %s", name)
			}
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				if less(b.StartLine, b.StartColumn, p.EndLine, p.EndColumn) {
					break
				}
				stack = stack[:len(stack)-1]
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				if less(p.EndLine, p.EndColumn, b.EndLine, b.EndColumn) {
					return fmt.Errorf("crossing block extents in %s", name)
				}
			}
			stack = append(stack, b)
		}
	}
	return nil
}
