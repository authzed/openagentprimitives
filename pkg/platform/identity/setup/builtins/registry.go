// Package builtins holds Go-coded setup flows for curated providers.
// Each flow registers itself with the registry under a name that
// matches the provider's `builtin:` field.
package builtins

import (
	"context"
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// Flow is a hand-coded credential-acquisition flow for one provider, expressed
// as a sequence of screens rather than as code that prompts.
//
// A flow owns no I/O: pkg/cli/tui's sequencer presents the screens and owns
// chrome, theme, and driver selection, decided once for every command instead of
// per provider. Branching lives in ordinary Go between screens (Screen.Prepare),
// which is what lets a caveat that applies to one provider be shown only to the
// users who hit it. Implementations must be cancellable via ctx.
type Flow interface {
	// Name matches the provider's `builtin:` field.
	Name() string

	// Screens describes the flow as a sequence. req carries what a flow needs to
	// compose its questions: the resolved provider config, user intent, the
	// AgentIdentity name and namespace, and the kind, so the flow knows whether it
	// is binding an env-var or a bearer.
	//
	// A nil error MUST come with at least one screen: a flow that asks nothing
	// would go on to store whatever an unanswered State yielded. A flow with
	// nothing to offer returns an error saying so instead.
	Screens(ctx context.Context, req Request) ([]tui.Screen, error)

	// Result reads the answered State and persists the credential through
	// req.Store. Called only after a successful run of the screens Screens
	// returned, on the same Flow value. It takes a ctx because Store is I/O
	// against the cluster: it writes the AgentIdentity's Secret.
	//
	// It is the fail-closed choke point every flow funnels through. huh's
	// accessible renderer cannot report a read error, so an input script that
	// runs out reaches here as silence rather than as a failure — a Result that
	// does not check its State before calling Store will persist an empty token.
	Result(ctx context.Context, req Request, st *tui.State) error

	// Verify live-checks a credential against the provider BEFORE it is stored.
	// Side-effect-free, non-interactive, safe to call from any surface (CLI, web,
	// tools). Failure modes map onto VerifyResult.Status — a returned error is
	// treated as indeterminate by callers, never as a rejection.
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
}

// Request is what the engine hands to a Flow's Screens and Result calls. It
// carries no I/O streams and no styler: what a flow describes is presented by
// the sequencer, over streams the command owns.
type Request struct {
	Provider   *provider.Provider
	UserIntent string // synthesized from class/spec

	// ScopeHint is the permission lines this run wants recommended, one per
	// line, replacing whatever the flow would otherwise derive from
	// UserIntent. Empty keeps the flow's own recommendation.
	//
	// It exists because a flow's guess is about the PROVIDER while the caller
	// knows what it will actually call, and the two can disagree in a way no
	// verification catches: github-pat recommends repository permissions,
	// which a caller enumerating an organization's members cannot use, and its
	// probe (GET /user) is answered by any token — so the wrong-permission
	// credential stores clean and 403s later.
	//
	// A caller supplying lines owes them a test against its own call surface.
	// These are printed to a human about to grant real access.
	ScopeHint []string

	Requirement  authkind.CredentialRequirement
	Kind         authkind.Kind   // matches binding-prefix (cli/toolspec/mcp)
	Target       authkind.Target // resolved target (toolkit / toolspec / mcpserver)
	Namespace    string
	IdentityName string        // AgentIdentity to mutate
	K8s          client.Client // for store callback to use

	// Store persists the credential value(s) into the AgentIdentity's Secret and
	// ensures the AgentIdentity has the credential + binding. Implementations call
	// this exactly once, from Result, on success.
	Store func(ctx context.Context, value StoreValue) error
}

// StoreValue is the credential-shape-agnostic input to Store.
type StoreValue struct {
	// Bearer is the token bytes for shape: bearer (PAT, generic API key).
	Bearer string

	// OAuth is the full token bundle for shape: oauth.
	OAuth *OAuthValue

	// KubeconfigYAML is the entire kubeconfig blob for shape: kubeconfig.
	KubeconfigYAML string
}

// OAuthValue carries everything needed to persist an oauth credential.
// Maps 1:1 to the OAuthCredentialSource Secret keys.
type OAuthValue struct {
	AccessToken   string
	RefreshToken  string
	ExpiresIn     int // seconds
	TokenEndpoint string
	ClientID      string
	// ClientSecret is persisted for confidential OAuth clients so the
	// refresh-token grant can authenticate the client. Empty for public
	// (PKCE-only) clients.
	ClientSecret string
	Scope        string
}

// Registry holds Flow implementations by name.
var (
	mu    sync.RWMutex
	flows = map[string]Flow{}
)

// Register adds f to the registry. Panics on duplicate name.
func Register(f Flow) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := flows[f.Name()]; dup {
		panic(fmt.Sprintf("builtins: duplicate flow %q", f.Name()))
	}
	flows[f.Name()] = f
}

// Get returns the Flow registered under name, or (nil, false).
func Get(name string) (Flow, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := flows[name]
	return f, ok
}

// All returns all registered flows.
func All() []Flow {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Flow, 0, len(flows))
	for _, f := range flows {
		out = append(out, f)
	}
	return out
}

// Reset clears the registry. For use in tests only.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	flows = map[string]Flow{}
}
