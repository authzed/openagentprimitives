package runner_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mimeOpinionScanDirs are the two directories that stand between the model
// registry and the provider call: the runner's hydration pass and channelsd's
// ingestion pipeline. They are the only places a second opinion about MIME
// types could plausibly grow, and they are deliberately named individually
// rather than scanning the whole repo — the point is a tight guard over the
// code path this feature owns, not a repo-wide lint.
//
// Relative to this package's directory, which is where `go test` runs.
var mimeOpinionScanDirs = []string{
	".",                                 // pkg/agent/runner
	"../../channels/channelsd/pipeline", // pkg/channels/channelsd/pipeline
}

// mimeLiteralPrefixes are the IANA top-level type prefixes that make a string
// literal a MIME type rather than an ordinary string. Prefix-matched against
// the literal's VALUE, so `strings.HasPrefix(mime, "image/")` and a bare
// "application/pdf" both trip it, while prose that merely mentions a type
// (including the doc comments in attachments.go that explain this very rule)
// does not — comments are not string literals, and the AST walk never sees
// them.
var mimeLiteralPrefixes = []string{
	"application/",
	"audio/",
	"image/",
	"text/",
	"video/",
}

// mimeLiteralExact are the same IANA top-level types WITHOUT the slash,
// matched exactly rather than by prefix. They close the hole the prefix set
// alone leaves: the natural spellings of a second opinion do not carry the
// slash. `strings.SplitN(mime, "/", 2)[0] == "image"` is a top-level-type
// check with no "image/" literal anywhere in it, and a MIME assembled from
// parts ("image" + "/" + sub) hides the whole string from a prefix scan.
// Both reintroduce exactly the opinion this file forbids while passing a
// prefix-only guard.
//
// Exact match, never prefix: "image"/"audio"/"video" as prefixes would fire on
// ordinary identifiers ("imageURL", "videoTag"), and "application" on
// "application/json"-adjacent prose. Exact match keeps false positives near
// zero in these two directories — neither has a legitimate bare "image" or
// "application" literal today.
//
// "text" is deliberately absent. `llm.ContentBlock{Type: "text"}` is the most
// common literal in both directories; including it would make the guard fire
// on nearly every file and force an escape hatch that would then be used to
// silence the real findings too.
//
// Still uncovered, honestly: a bare SUBTYPE with no top-level type in it —
// `strings.Contains(mime, "pdf")`. There is no low-false-positive literal set
// for subtypes ("gif", "png", "webp", …, plus every future one), and a guard
// that fires on ordinary words would be turned off. This guard is structural,
// not exhaustive: it makes the shapes people actually reach for fail loudly.
var mimeLiteralExact = []string{
	"application",
	"audio",
	"image",
	"video",
}

// TestNoMIMEPatternMatchingOutsideRegistry is the guard that keeps native
// format support a ONE-ROW EDIT to pkg/agent/llm/models/models.go.
//
// The registry maps each MIME a model accepts to the native block type it
// rides in, precisely so no consumer ever has to infer either. A
// `strings.HasPrefix(mime, "image/")` — or any other hardcoded MIME — in the
// runner or the ingestion pipeline silently reintroduces a second opinion
// that the registry CANNOT override: the model row would declare the new
// format, and the consumer would still refuse to carry it, or carry it as the
// wrong block type. The failure is silent in exactly the workflow this
// feature advertises, which is why it is asserted structurally rather than
// left to review.
//
// The escape valve is not an allowlist. A literal that trips this guard
// belongs in the registry (if it is a decision about what a model accepts) or
// behind a named constant in the package that genuinely owns it (if it is
// something else entirely).
//
// That second case is real and will happen: an HTTP handler added to either
// scanned directory with a legitimate `w.Header().Set("Content-Type",
// "application/json")` fires this guard, and it is not wrong to. The right
// answer is the named constant, declared in the package that owns that
// concern — outside these two directories — and referenced here by name. The
// WRONG answer is narrowing mimeOpinionScanDirs: dropping a directory to
// silence one honest literal disables the guard for every file in it,
// including the second opinion someone adds next year, and nothing will say
// so. Narrow the list only when a directory genuinely no longer sits between
// the registry and the provider call.
func TestNoMIMEPatternMatchingOutsideRegistry(t *testing.T) {
	type finding struct {
		pos     string
		literal string
	}
	var findings []finding

	// One entry per scanned directory, checked AFTER every directory has been
	// walked — see the loop below the walk for why the checks cannot live
	// inside it.
	type dirStatus struct {
		dir     string
		err     error
		scanned int
	}
	var statuses []dirStatus

	for _, dir := range mimeOpinionScanDirs {
		scanned := 0
		fset := token.NewFileSet()

		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			scanned++

			ast.Inspect(f, func(n ast.Node) bool {
				// Import paths are string literals too, and "image/png" /
				// "text/template" are real stdlib packages. Skipping the whole
				// ImportSpec is what keeps a legitimate import from reading as
				// a MIME opinion.
				if _, ok := n.(*ast.ImportSpec); ok {
					return false
				}
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				val, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if isMIMEOpinion(val) {
					findings = append(findings, finding{
						pos: fset.Position(lit.Pos()).String(), literal: val,
					})
				}
				return true
			})
			return nil
		})
		statuses = append(statuses, dirStatus{dir: dir, err: err, scanned: scanned})
	}

	// Findings first, status second. Both are reported for EVERY directory,
	// because a require inside the walk loop aborted the test on the first
	// stale path — throwing away findings already collected from directories
	// that scanned fine, and never scanning the ones after it. The staleness
	// is real and must fail, but it must not be the reason a genuine second
	// opinion goes unreported.
	for _, f := range findings {
		assert.Fail(t, "MIME type named outside the model registry",
			"%s: %q\n\n"+
				"Native format support must be decided ONLY by pkg/agent/llm/models/models.go, which maps each\n"+
				"MIME to the native block type it rides in. A MIME named here is a SECOND OPINION the registry\n"+
				"cannot override: adding the format to a model's row would no longer be sufficient to send it\n"+
				"natively — that one-row edit would silently do nothing, or carry the file as the wrong block\n"+
				"type — and nothing would fail until a user's file quietly stopped reaching the model.\n\n"+
				"Move the decision into the registry, or, if this literal is genuinely not about what a model\n"+
				"accepts, put it behind a named constant in the package that owns that concern — do NOT narrow\n"+
				"mimeOpinionScanDirs, which silently disables this guard for every other file in that directory.",
			f.pos, f.literal)
	}

	// A walk error (most likely a stale relative path after a package move) or
	// a directory that yielded no files must fail loudly: a guard that
	// silently scans nothing is worse than no guard, because it reads as
	// passing forever.
	for _, st := range statuses {
		require.NoError(t, st.err, "walking %s for MIME literals", st.dir)
		require.NotZero(t, st.scanned, "scanned no non-test Go files under %s — the guard's directory list is stale and it is silently checking nothing", st.dir)
	}
}

// isMIMEOpinion reports whether a string literal is a MIME type or a MIME
// top-level type — the two shapes a second opinion about what a model accepts
// can take. See mimeLiteralPrefixes and mimeLiteralExact for why one set is
// prefix-matched and the other is not.
func isMIMEOpinion(val string) bool {
	for _, p := range mimeLiteralPrefixes {
		if strings.HasPrefix(val, p) {
			return true
		}
	}
	for _, e := range mimeLiteralExact {
		if val == e {
			return true
		}
	}
	return false
}
