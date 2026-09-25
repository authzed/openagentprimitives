// Package apiadapter is the declarative HTTP-API adapter: a config describing
// a REST API's operations, and an executor that performs one of them.
//
// It runs NO user code. A config is data — a base URL, an auth scheme, and a
// list of operations with their parameters. The adapter binds call arguments
// into an HTTP request, injects one credential, and returns the response. There
// is no templating that can execute and no code generation anywhere in it.
package apiadapter

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/net/http/httpguts"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// MaxConfigBytes caps a config document; AP_SIDECAR_CONFIG rides in the pod
// environment, and an unbounded document would be an unbounded env var.
const MaxConfigBytes = 64 << 10

// validOperationName bounds an Operation.Name: synthesis normalization
// already allows up to 128 chars, but permsurface prefixes it (`<ref>_`)
// into a 64-char handle — 48 leaves that prefix room. A name this shape also
// never needs escaping anywhere it is later used as an identifier.
var validOperationName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,48}$`)

// Config is a whole adapter configuration: one API, its auth, its operations.
type Config struct {
	// BaseURL is the API root every operation path is joined onto. Absolute,
	// http(s), host guarded by safehttp.GuardHost at Validate time (a static
	// pre-check; the guarded dialer is the authoritative runtime backstop).
	BaseURL string `json:"baseURL"`
	// Auth is how each request authenticates. Exactly one scheme.
	Auth Auth `json:"auth"`
	// Operations each become one MCP tool. Names are unique.
	Operations []Operation `json:"operations"`
}

// Auth names the scheme and where the credential comes from. The credential
// VALUE is never in the config — it arrives at runtime from EnvVar, which the
// SidecarToolbox's upstreamAuth populates.
type Auth struct {
	// Type is "none" | "bearer" | "header" | "basic" | "query".
	Type string `json:"type"`
	// EnvVar names the environment variable holding the credential. Required
	// for every Type except "none".
	EnvVar string `json:"envVar,omitempty"`
	// Name is the header name (Type "header") or query parameter name
	// (Type "query"). Required for those two, ignored otherwise.
	Name string `json:"name,omitempty"`
}

// Operation is one API call, exposed as one MCP tool.
type Operation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Method      string `json:"method"`
	// Path is joined onto BaseURL. "{param}" segments bind from an "in: path"
	// parameter of the same name.
	Path   string  `json:"path"`
	Params []Param `json:"params,omitempty"`
}

// Param is one argument of an operation and where it goes on the wire.
type Param struct {
	Name string `json:"name"`
	// In is "path" | "query" | "header" | "body".
	In       string `json:"in"`
	Required bool   `json:"required,omitempty"`
	// Type is the JSON-schema scalar: "string" | "number" | "integer" |
	// "boolean". Body params may also be "object" or "array".
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

var (
	validMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	validIn      = map[string]bool{"path": true, "query": true, "header": true, "body": true}
	validAuth    = map[string]bool{"none": true, "bearer": true, "header": true, "basic": true, "query": true}
	validTypes   = map[string]bool{"string": true, "number": true, "integer": true, "boolean": true, "object": true, "array": true}
)

// Parse decodes a config from YAML or JSON (YAML is a superset, so one path
// handles both) and validates it. A config that parses but does not validate is
// returned as an error, never as a usable Config.
func Parse(data []byte) (Config, error) {
	if len(data) > MaxConfigBytes {
		return Config{}, fmt.Errorf("apiadapter: config is %d bytes, exceeds the %d byte limit", len(data), MaxConfigBytes)
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return Config{}, fmt.Errorf("apiadapter: parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate reports the first structural problem with the config.
func (c Config) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("apiadapter: baseURL %q must be an absolute URL", c.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("apiadapter: baseURL scheme %q must be http or https", u.Scheme)
	}
	// Operation paths are joined onto BaseURL by string concatenation
	// (buildRequest), not url.ResolveReference: a query or fragment on the
	// base is either silently dropped or ends up overwriting the operation's
	// own query, and an opaque ("scheme:opaque") baseURL has no host for a
	// path to join onto at all (defense-in-depth here — such a baseURL is
	// already refused above for having an empty Host).
	if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("apiadapter: baseURL %q must carry no query or fragment (operation paths are joined onto it)", c.BaseURL)
	}
	// A URL's userinfo (`user:pass@host`) is an inline credential: Go's
	// http.send sets an Authorization: Basic header from it whenever the
	// request carries none — exactly the auth.type "none" case — so this
	// would be a working credential channel the config format has no other
	// guard against. The credential comes from auth.envVar, never the config.
	if u.User != nil {
		return fmt.Errorf("apiadapter: baseURL %q must carry no userinfo; the credential comes from auth.envVar, never the config", c.BaseURL)
	}
	if err := safehttp.GuardHost(u.Hostname()); err != nil {
		return fmt.Errorf("apiadapter: baseURL host: %w", err)
	}
	if err := c.Auth.validate(); err != nil {
		return err
	}
	if u.Scheme == "http" && c.Auth.Type != "none" {
		return fmt.Errorf("apiadapter: baseURL scheme %q with auth.type %q would send the credential in cleartext; use https", u.Scheme, c.Auth.Type)
	}
	if len(c.Operations) == 0 {
		return fmt.Errorf("apiadapter: at least one operation is required")
	}
	seen := map[string]bool{}
	for i, op := range c.Operations {
		if err := op.validate(); err != nil {
			return fmt.Errorf("apiadapter: operations[%d]: %w", i, err)
		}
		if seen[op.Name] {
			return fmt.Errorf("apiadapter: duplicate operation name %q", op.Name)
		}
		seen[op.Name] = true
	}
	return nil
}

func (a Auth) validate() error {
	if !validAuth[a.Type] {
		return fmt.Errorf("apiadapter: auth.type %q must be one of none, bearer, header, basic, query", a.Type)
	}
	if a.Type != "none" && a.EnvVar == "" {
		return fmt.Errorf("apiadapter: auth.envVar is required for auth.type %q", a.Type)
	}
	if (a.Type == "header" || a.Type == "query") && a.Name == "" {
		return fmt.Errorf("apiadapter: auth.name is required for auth.type %q", a.Type)
	}
	if a.Type == "header" && !httpguts.ValidHeaderFieldName(a.Name) {
		return fmt.Errorf("apiadapter: auth.name %q is not a valid HTTP header field name", a.Name)
	}
	return nil
}

func (o Operation) validate() error {
	if !validOperationName.MatchString(o.Name) {
		return fmt.Errorf("name %q must match %s", o.Name, validOperationName.String())
	}
	if !validMethods[o.Method] {
		return fmt.Errorf("method %q must be one of GET, POST, PUT, PATCH, DELETE", o.Method)
	}
	if !strings.HasPrefix(o.Path, "/") {
		return fmt.Errorf("path %q must start with /", o.Path)
	}
	if err := validatePathBraces(o.Path); err != nil {
		return err
	}
	byName := map[string]Param{}
	for _, p := range o.Params {
		if p.Name == "" {
			return fmt.Errorf("param name is required")
		}
		if !validIn[p.In] {
			return fmt.Errorf("param %q: in %q must be one of path, query, header, body", p.Name, p.In)
		}
		if p.In == "header" && !httpguts.ValidHeaderFieldName(p.Name) {
			return fmt.Errorf("param %q: name is not a valid HTTP header field name", p.Name)
		}
		if !validTypes[p.Type] {
			return fmt.Errorf("param %q: type %q is not a supported type", p.Name, p.Type)
		}
		if (p.Type == "object" || p.Type == "array") && p.In != "body" {
			return fmt.Errorf("param %q: type %q is only valid for in: body", p.Name, p.Type)
		}
		if byName[p.Name].Name != "" {
			return fmt.Errorf("duplicate param %q", p.Name)
		}
		byName[p.Name] = p
	}
	// Every {seg} in the path must have a path param, and every path param
	// must appear in the path — a mismatch is a config bug that would
	// otherwise surface as a 404 at call time.
	for _, seg := range pathParams(o.Path) {
		p, ok := byName[seg]
		if !ok {
			return fmt.Errorf("path placeholder {%s} has no matching param", seg)
		}
		if p.In != "path" {
			return fmt.Errorf("param %q is used as a path placeholder but declares in: %s", seg, p.In)
		}
		if !p.Required {
			return fmt.Errorf("path param %q must be required", seg)
		}
	}
	inPath := map[string]bool{}
	for _, seg := range pathParams(o.Path) {
		inPath[seg] = true
	}
	for _, p := range o.Params {
		if p.In == "path" && !inPath[p.Name] {
			return fmt.Errorf("param %q declares in: path but {%s} is not in the path", p.Name, p.Name)
		}
	}
	return nil
}

// validatePathBraces rejects a path whose '{'/'}' are not correctly paired: a
// second '{' before the first has closed, or a '{' with no closing '}' at
// all. pathParams (below) treats such a path leniently — it just stops
// extracting at the first unmatched '{' — so this runs first and catches
// what that silent partial extraction would otherwise hide.
func validatePathBraces(path string) error {
	open := false
	for _, r := range path {
		switch r {
		case '{':
			if open {
				return fmt.Errorf("path %q has an unbalanced '{'", path)
			}
			open = true
		case '}':
			open = false
		}
	}
	if open {
		return fmt.Errorf("path %q has an unbalanced '{'", path)
	}
	return nil
}

// pathParams returns the {placeholder} names in a path, in order.
func pathParams(path string) []string {
	var out []string
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			return out
		}
		j := strings.Index(path[i:], "}")
		if j < 0 {
			return out
		}
		out = append(out, path[i+1:i+j])
		path = path[i+j+1:]
	}
}

// Operation returns the named operation.
func (c Config) Operation(name string) (Operation, bool) {
	for _, op := range c.Operations {
		if op.Name == name {
			return op, true
		}
	}
	return Operation{}, false
}

// OperationNames returns every operation name, sorted — the set a
// SidecarToolbox's spec.tools allowlist must equal (plan 7b's webhook).
func (c Config) OperationNames() []string {
	out := make([]string, 0, len(c.Operations))
	for _, op := range c.Operations {
		out = append(out, op.Name)
	}
	sort.Strings(out)
	return out
}

// ToolNamesMatch reports whether declaredToolNames — a SidecarToolbox's
// spec.tools names — names exactly c's operations, one-for-one.
// declaredToolNames is deduplicated before comparing: spec.tools carries no
// CRD- or Go-level uniqueness constraint (no listMapKey on MCPServerTool), so
// a duplicated-but-matching name must not fail the set-equality check below.
// Returns "" when the sets match, or a message naming both sides.
//
// This is the ONE comparison both the admission webhook's checkSidecarConfig
// and the CLI's SidecarToolbox ValidateFile run — shared here so an author
// sees the identical refusal locally that the cluster would give at
// admission, rather than two independently-worded (and independently
// maintained) checks that could silently drift apart.
func (c Config) ToolNamesMatch(declaredToolNames []string) string {
	ops := c.OperationNames() // sorted; Validate already rejects a duplicate operation name.
	seen := make(map[string]bool, len(declaredToolNames))
	declared := make([]string, 0, len(declaredToolNames))
	for _, name := range declaredToolNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		declared = append(declared, name)
	}
	sort.Strings(declared)
	if !slices.Equal(ops, declared) {
		return fmt.Sprintf("SidecarToolbox.spec.tools must name exactly the config's operations: config has %v, tools declare %v", ops, declared)
	}
	return ""
}

// UpstreamEnvVarMismatch reports whether upstreamEnvVar — a SidecarToolbox's
// spec.upstreamAuth.envVar — disagrees with the name this config's own
// auth.envVar declares. A mismatch passes both validate_spec and admission
// today (each checks its own half of the config separately) and only fails at
// sidecar boot (internal/cmd/apiadapter/main.go), when the adapter reads its
// credential from the env var the CONFIG names and finds nothing there — the
// exact gap this method closes by making both callers check it before either
// admits the object. This is the ONE comparison both the admission webhook's
// checkSidecarConfig and the CLI's SidecarToolbox ValidateFile run, mirroring
// ToolNamesMatch above.
//
// Returns "" when they agree, or when auth.type is "none" — a config that
// needs no credential has nothing for upstreamEnvVar to agree or disagree
// with, so it is not checked in that case.
func (c Config) UpstreamEnvVarMismatch(upstreamEnvVar string) string {
	if c.Auth.Type == "none" {
		return ""
	}
	if upstreamEnvVar != c.Auth.EnvVar {
		return fmt.Sprintf("SidecarToolbox.spec.upstreamAuth.envVar %q must match the config's auth.envVar %q: a mismatch would pass validation and admission but fail at sidecar boot", upstreamEnvVar, c.Auth.EnvVar)
	}
	return ""
}
