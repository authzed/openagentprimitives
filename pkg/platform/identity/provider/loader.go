package provider

import (
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"sync"

	"sigs.k8s.io/yaml"

	embedfs "github.com/authzed/openagentprimitives/providers"
)

var (
	once    sync.Once
	provs   []Provider
	byID    map[string]*Provider
	loadErr error
)

// All returns every embedded provider. Sorted by ID. Panics on parse
// errors (build-bug).
func All() []Provider {
	once.Do(load)
	checkErr()
	return provs
}

// ByID returns the provider with the given ID, or (nil, false).
func ByID(id string) (*Provider, bool) {
	once.Do(load)
	checkErr()
	p, ok := byID[id]
	return p, ok
}

func checkErr() {
	if loadErr != nil {
		panic(fmt.Sprintf("provider: failed to load embedded providers: %v", loadErr))
	}
}

func load() {
	provs, byID, loadErr = loadFrom(embedfs.FS)
}

// loadFrom parses and validates every provider YAML in fsys. It is split out
// of load() so the validation the loader enforces can be driven from a test
// with a synthetic catalog: a rule that is correct but never called fails
// nothing, and the embedded FS cannot be made to contain a bad provider.
func loadFrom(fsys fs.FS) ([]Provider, map[string]*Provider, error) {
	var provs []Provider
	byID := map[string]*Provider{}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("read providers FS: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var p Provider
		if err := yaml.Unmarshal(data, &p); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if p.ID == "" {
			return nil, nil, fmt.Errorf("%s: provider id is empty", e.Name())
		}
		if _, dup := byID[p.ID]; dup {
			return nil, nil, fmt.Errorf("%s: duplicate provider id %q", e.Name(), p.ID)
		}
		// A declared token-format regex is the user-visible source of truth
		// enforced at every entry point (ValidateToken). Fail loudly here if
		// an embedded provider ships a pattern that doesn't compile — the
		// alternative is a silent no-op gate that lets wrong tokens through.
		if p.TokenShape != nil && p.TokenShape.Pattern != "" {
			if _, err := regexp.Compile(p.TokenShape.Pattern); err != nil {
				return nil, nil, fmt.Errorf("%s: tokenShape pattern %q does not compile: %w", e.Name(), p.TokenShape.Pattern, err)
			}
		}
		// A declared verify: probe is executed with real credentials at
		// every entry point — fail loudly at load if the config is
		// malformed rather than silently probing a garbage URL.
		if p.Verify != nil {
			if err := validateVerifyConfig(p.Verify); err != nil {
				return nil, nil, fmt.Errorf("%s: verify config: %w", e.Name(), err)
			}
		}
		// A declared authFailure: block is consulted to corroborate an
		// agent's claim that a credential died — fail loudly at load if a
		// stderrPatterns entry doesn't compile or a status/exit code is out
		// of range, rather than shipping a corroboration rule that can
		// never match (or worse, panics at match time).
		if p.AuthFailure != nil {
			if err := validateAuthFailureConfig(p.ID, p.AuthFailure); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
		}
		// Cross-field rule, so it is checked for EVERY provider rather than
		// only those with an authFailure: block — an agent-shaped signal
		// (stderrPatterns or exitCodes) without a verify: probe makes a
		// forgeable signal decisive. See
		// validateAgentShapedCorroborationHasProbe for the full argument.
		if err := validateAgentShapedCorroborationHasProbe(p); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		provs = append(provs, p)
		byID[p.ID] = &provs[len(provs)-1]
	}

	// Sort by ID so All() returns a stable, deterministic order. The
	// byID pointers are stale after the sort, so rebuild the map.
	sort.Slice(provs, func(i, j int) bool { return provs[i].ID < provs[j].ID })
	byID = make(map[string]*Provider, len(provs))
	for i := range provs {
		byID[provs[i].ID] = &provs[i]
	}
	return provs, byID, nil
}

// validateVerifyConfig enforces the structural rules for a verify: block:
// an absolute https endpoint, a known method, and a known auth scheme.
func validateVerifyConfig(v *VerifyConfig) error {
	if v.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	u, err := url.Parse(v.Endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q does not parse: %w", v.Endpoint, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("endpoint %q must be https with a host", v.Endpoint)
	}
	if v.Method != "" && !slices.Contains([]string{"GET", "HEAD", "POST"}, v.Method) {
		return fmt.Errorf("method %q not allowed (GET, HEAD, POST)", v.Method)
	}
	if v.AuthScheme != "" && v.AuthScheme != "token" && v.AuthScheme != "Bearer" {
		return fmt.Errorf("authScheme %q not allowed (token, Bearer)", v.AuthScheme)
	}
	return nil
}

// ByBuiltin returns the provider whose Builtin field names flow, or
// (nil, false).
//
// It exists because the link between a setup flow and its catalog entry runs
// that way round — a provider DECLARES which builtin handles it — and every
// shipped provider happening to share its id with its builtin's name makes
// the shortcut of looking a flow up ByID silently correct today and wrong the
// first time a flow serves a provider under a different name. A caller
// holding only a flow name (the CLI's directory wizard, which gets one from a
// relsync kind) needs the declared direction, not the coincidence.
//
// Ambiguity is (nil, false), not a first match: a generic flow that several
// providers declare — oauth-mcp is the shape — has no single provider to
// stand for it, and picking one would hand a caller another provider's token
// shape and verification probe.
func ByBuiltin(flow string) (*Provider, bool) {
	once.Do(load)
	checkErr()
	if flow == "" {
		return nil, false
	}
	var found *Provider
	for i := range provs {
		if provs[i].Builtin != flow {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = &provs[i]
	}
	return found, found != nil
}
