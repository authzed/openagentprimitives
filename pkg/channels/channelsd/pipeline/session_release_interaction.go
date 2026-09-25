// pkg/channels/channelsd/pipeline/session_release_interaction.go
//
// The bound decision handler for the session_release interaction category
// (releasing a forensic SessionHold — see
// pkg/channels/channelinteractions/categories/sessionrelease), registered the
// same way every other bound handler in this package is: a Bind*Handler
// function called once at channelsd process start
// (internal/cmd/channelsd/main.go), mirroring permission_interaction.go's
// BindPermissionHandler most closely — session_release shares
// permission_request's exact shape (DecideOwner, no park, no runner resume).
//
// The decision-application logic itself is NOT duplicated here. It already
// lives in pkg/controllers/sessionhold.Reconciler.Decide, built and
// unit-tested in isolation (finding the session's active hold, clearing
// standing plan approvals BEFORE releasing, applying refuse/approve). This
// file only constructs that Reconciler from fields this Pipeline already
// carries — K8s and Mem, the same client and memory facade every other bound
// handler in this package uses — and wires it into the registry. No new
// dependency path into channelsd: everything Decide needs was already wired
// here for other handlers.
//
// Standing is NOT re-checked here: HandleInteractionDecision
// (interaction_decision.go) already ran the category's DeciderPolicy check
// (session_release = DecideOwner -> the session's approve-set) before
// invoking the bound handler.
package pipeline

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories/sessionrelease"
	"github.com/authzed/openagentprimitives/pkg/controllers/sessionhold"
)

// BindSessionReleaseHandler wires decideSessionRelease into the interaction
// registry as the bound decision handler for sessionrelease.CategoryName.
// Called once at channelsd process start, mirroring BindPermissionHandler's
// call site in internal/cmd/channelsd/main.go.
func BindSessionReleaseHandler(p *Pipeline) {
	channelinteractions.Bind(sessionrelease.CategoryName, p.decideSessionRelease)
}

// decideSessionRelease applies a resolved session_release decision by
// delegating to pkg/controllers/sessionhold.Reconciler.Decide, constructed
// fresh per call from this Pipeline's own K8s client and memory facade.
// Building it here rather than caching one on Pipeline keeps this file the
// single place that couples channelsd to sessionhold.Reconciler's field
// names, and it costs nothing — the Reconciler holds no state of its own
// beyond these two fields.
func (p *Pipeline) decideSessionRelease(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	r := &sessionhold.Reconciler{Client: p.K8s, Memory: p.Mem}
	return r.Decide(ctx, d)
}
