// Package providers is the LLM-provider factory: it maps a provider name
// (from AgentSession EffectiveSettings.Model.Provider, a CRD string) to a
// constructed llm.Provider. Each provider package registers itself via init();
// binaries blank-import the providers they need. This is the AGENTS.md
// "registry, not switch" seam for a value dispatched by a CRD string — it
// replaces the former hardcoded anthropic.New at the runner.
package providers

import (
	"context"
	"fmt"
	"sort"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// Factory constructs a provider from a raw credential string — today the
// statically-mounted key/bearer.
type Factory func(apiKey string) (llm.Provider, error)

// defaultProvider is used when Model.Provider is empty.
const defaultProvider = "anthropic"

var registry = map[string]Factory{}
var rotating = map[string]bool{}

// RegisterRotating declares a provider whose access credential must be resolved
// again for every request. The refresh token itself stays with its owner.
func RegisterRotating(name string, f Factory) {
	Register(name, f)
	rotating[name] = true
}

func RotatesCredentials(name string) bool { return rotating[name] }

// WithCredentialSource retains provider metadata while reading the current
// access token before each inference. Read failures fail closed.
func WithCredentialSource(p llm.Provider, source func(context.Context) (string, error)) llm.Provider {
	if !RotatesCredentials(p.Name()) {
		return p
	}
	return &refreshingProvider{Provider: p, source: source}
}

type refreshingProvider struct {
	llm.Provider
	source func(context.Context) (string, error)
}

func (p *refreshingProvider) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	if p.source == nil {
		return llm.Response{}, fmt.Errorf("model credential source is not configured")
	}
	token, err := p.source(ctx)
	if err != nil {
		return llm.Response{}, fmt.Errorf("resolve current model credential: %w", err)
	}
	current, err := New(p.Name(), token)
	if err != nil {
		return llm.Response{}, err
	}
	return current.Send(ctx, req)
}

// Register adds a provider factory under name. Called from provider packages'
// init(). Panics on duplicate or empty name — both are programmer errors
// caught at process start, not runtime conditions.
func Register(name string, f Factory) {
	if name == "" {
		panic("providers.Register: empty name")
	}
	if _, dup := registry[name]; dup {
		panic("providers.Register: duplicate provider " + name)
	}
	registry[name] = f
}

// New returns the llm.Provider for the given provider name using apiKey as the
// credential. Empty name resolves to the anthropic default. Unknown name
// returns a nil interface + error (never a typed-nil).
func New(name, apiKey string) (llm.Provider, error) {
	if name == "" {
		name = defaultProvider
	}
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("providers: unknown LLM provider %q (registered: %v)", name, registered())
	}
	return f(apiKey)
}

func registered() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
