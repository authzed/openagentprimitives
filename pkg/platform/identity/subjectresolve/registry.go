package subjectresolve

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// mu guards resolvers and seenForms. An ordinary slice (not
// pkg/x/kindregistry) is used deliberately: kindregistry.All() sorts by key,
// which would scramble the "specific schemes first, generic fallback last"
// order that both dispatch (Resolve) and documentation (Usages) depend on.
var (
	mu        sync.RWMutex
	resolvers []Resolver
	seenForms = map[string]bool{}
)

func init() {
	// Explicit, ordered wiring — not one init() per resolver file — so the
	// dispatch/documentation order is a property of this list, not of
	// whatever order the Go toolchain happens to process email.go,
	// resource.go, and triggerauthor.go in.
	Register(emailResolver{})
	Register(triggerAuthorResolver{})
	Register(resourceResolver{}) // fallback: must stay last
}

// Register adds a resolver, in call order. Panics (at init time, like
// sibling registries) when the resolver's Usage() reports an empty form or
// description — a resolver cannot exist undocumented — or when its form is
// already registered.
func Register(r Resolver) {
	form, desc := r.Usage()
	if form == "" {
		panic("subjectresolve: resolver registered with an empty Usage() form")
	}
	if desc == "" {
		panic(fmt.Sprintf("subjectresolve: resolver for form %q registered with an empty Usage() description", form))
	}
	mu.Lock()
	defer mu.Unlock()
	if seenForms[form] {
		panic(fmt.Sprintf("subjectresolve: duplicate registration for form %q", form))
	}
	seenForms[form] = true
	resolvers = append(resolvers, r)
}

// Usages lists every registered resolver's form and description, in
// registration order — specific schemes first, the generic fallback last.
// This is the single source `get_preferences`'s tool-schema documentation
// derives from; a new resolver is automatically reflected here.
func Usages() []Usage {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Usage, 0, len(resolvers))
	for _, r := range resolvers {
		form, desc := r.Usage()
		out = append(out, Usage{Form: form, Description: desc})
	}
	return out
}

// Resolve resolves ref against every registered resolver, in registration
// order. The first resolver that claims ref (matched=true) commits the
// outcome, even when unresolved. A RelationReader/SessionAnnotations error
// from within a resolver is returned to the caller, never swallowed into an
// "unresolved" Resolution. When no resolver claims ref, it is rejected with
// a Reason naming the supported forms.
func Resolve(ctx context.Context, ref string, env Env) (Resolution, error) {
	mu.RLock()
	snap := make([]Resolver, len(resolvers))
	copy(snap, resolvers)
	mu.RUnlock()

	for _, r := range snap {
		res, matched, err := r.TryResolve(ctx, ref, env)
		if !matched {
			continue
		}
		return res, err
	}
	return Resolution{Reason: unsupportedFormReason(ref)}, nil
}

// unsupportedFormReason names the registered forms so the message can never
// drift from what Usages() (and so the tool schema) actually advertises.
func unsupportedFormReason(ref string) string {
	usages := Usages()
	forms := make([]string, 0, len(usages))
	for _, u := range usages {
		forms = append(forms, u.Form)
	}
	return fmt.Sprintf("%q is not a supported user reference; supported forms: %s", truncateRef(ref), strings.Join(forms, ", "))
}

// maxEchoedRefRunes bounds how much of an agent-supplied reference a Reason
// echoes back. Reasons are relayed to channels and recorded in transcripts;
// an unbounded echo would let one malformed reference bloat both.
const maxEchoedRefRunes = 200

// truncateRef caps an agent-supplied reference before it is echoed into a
// Reason, marking the cut with an ellipsis.
func truncateRef(ref string) string {
	runes := []rune(ref)
	if len(runes) <= maxEchoedRefRunes {
		return ref
	}
	return string(runes[:maxEchoedRefRunes]) + "…"
}
