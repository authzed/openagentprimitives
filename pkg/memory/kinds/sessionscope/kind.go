// Package sessionscope is the memory Kind for the dynamic session scope
// (Layer 2). One Entry per AgentSession, stable ID "scp-config". Bumping
// ScopeVersion on each applied delta lets the runner invalidate caches.
package sessionscope

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// KindName is the registered name of this memory Kind.
const KindName = "session_scope"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "scp-" }

// WriteAuthority is SessionWritten because this kind has TWO writers with
// different credentials: authzd applies approved scope deltas
// (internal/cmd/authzd/metaagent.go) and the runner writes the class defaults it bound
// at cold start (pkg/authz/bind_defaults.go, from internal/cmd/runner/main.go).
//
// So a session can already rewrite its own scope directly — which is why the
// metaagent's gate-flag findings are defense-in-depth, not a live bypass.
// ComponentWritten would break BindClassDefaults without closing that, and
// would close less than it looks: authzd's ApplyScopeChange is a second,
// legitimate writer that authenticates as a component and passes the per-kind
// door either way.
//
// Moving the default-binding write to the operator — the step that would let
// this line flip — is a redesign, not a relocation. BindClassDefaults is not a
// projection of the AgentClass: it runs a live SpiceDB CheckToolCall per
// declared default, against the session's resolved auth subject, through the
// runner's checker + ZedToken cache (pkg/authz/engine/engine.go), which the
// AgentSession reconciler does not hold. And scope.ApplyDelta bumps
// ScopeVersion unconditionally, because the bump IS the ack to the runner's
// scope-cache invalidators — so a reconciler rewriting this record every pass
// would bump it every pass, and the move needs a once-only guard before it is
// safe at all.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(scope.Scope{}) }
func (Kind) IndexedFields() []string                        { return []string{"scopeVersion"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
