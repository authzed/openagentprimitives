package hooks

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// interactCheckErrorPrefix marks a Deny that arose from a TRANSIENT SpiceDB
// check failure (or an unwired checker), as opposed to a definitive
// not-authorized. The channelsd host inspects the deny reason via
// IsInteractCheckError so a transient error keeps OutcomeInternalError
// (drop/retry) semantics rather than posting a permission request the way a
// definitive deny does.
const interactCheckErrorPrefix = "interact-check-error: "

// IsInteractCheckError reports whether a Deny reason produced by the Interact
// hook represents a transient check failure (vs. a definitive not-authorized).
func IsInteractCheckError(reason string) bool {
	return strings.HasPrefix(reason, interactCheckErrorPrefix)
}

// InteractDeps is the dependency struct for the Interact hook.
//
// CheckInbound answers "may this actor put a turn into this session" for an
// actor of ANY subject type — engine.CheckSessionInbound dispatches a
// user:<canonical> to agentsession#interact and an agentsession:<ns>/<name> to
// agentsession#converse. The hook deliberately takes the whole typed subject
// rather than a bare canonical id: a bare id cannot express which of those two
// questions is being asked, and collapsing them is exactly how a delegated
// child's inbound came to be checked as `user:agentsession:<ns>/<name>` — a
// request SpiceDB rejects with InvalidArgument, because an object id may not
// contain ':'. That surfaced here as a transient check failure, so the inbound
// was dropped as an infrastructure fault rather than reported as unauthorized.
//
// The caller MUST bind fullyConsistent=true (see plan Risk 4 — the documented
// stale-read production failure).
type InteractDeps struct {
	CheckInbound func(ctx context.Context, ns, name string, actor identity.Subject) (bool, error)
	Logger       *slog.Logger
}

// Interact is the InboundTurn hook that runs the cheap SpiceDB check before a
// message reaches the runner. Allow on a positive check; Deny on a definitive
// not-authorized; Deny (fail-closed, marked transient) on a check error or
// unwired checker. The channelsd Host routes a definitive Deny to the
// pipeline's handlePermissionDeny (blocklist + permission_request publish) and
// a transient-error Deny to OutcomeInternalError.
//
// An actor whose subject type has no inbound permission at all
// (authz.ErrActorTypeUnsupported) is a DEFINITIVE deny, not a transient one:
// no tuple anyone could write would change the answer, so retrying it forever
// is the wrong shape. It is reported as an ordinary Deny with the cause in the
// reason.
type Interact struct{ d InteractDeps }

// NewInteract constructs an Interact hook.
func NewInteract(d InteractDeps) *Interact {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Interact{d: d}
}

func (h *Interact) Name() string             { return "interact" }
func (h *Interact) Points() []pipeline.Point { return []pipeline.Point{pipeline.InboundTurn} }

func (h *Interact) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if h.d.CheckInbound == nil {
		// No checker wired: fail closed but mark transient — an unwired checker is
		// an infra misconfiguration, not a user not-authorized.
		h.d.Logger.Info("interact: CheckInbound not wired; denying (fail-closed, transient)",
			"session", in.Session.String())
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  interactCheckErrorPrefix + "CheckInbound not wired",
		}
	}

	// SubjectRef, not Subject: in.Requester holds a bare base64 canonical for a
	// human and an already-qualified "<type>:<id>" for every non-human acting
	// subject a Channel can assert (identity.CanonicalUserID.SubjectRef). Subject
	// would prefix "user:" onto both and turn the second into a user object
	// naming nobody.
	actor := in.Requester.SubjectRef()

	ok, err := h.d.CheckInbound(ctx, in.Session.Namespace, in.Session.Name, actor)
	if err != nil {
		if errors.Is(err, authz.ErrActorTypeUnsupported) {
			h.d.Logger.Info("interact: acting subject type cannot message a session; denying (definitive)",
				"session", in.Session.String(), "actor", actor.String(), "err", err.Error())
			return pipeline.Decision{
				Verdict: pipeline.Deny,
				Reason:  "interact: " + err.Error(),
			}
		}
		h.d.Logger.Info("interact: check errored; denying (fail-closed, transient)",
			"session", in.Session.String(), "actor", actor.String(), "err", err.Error())
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  interactCheckErrorPrefix + err.Error(),
		}
	}
	if !ok {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  "interact: subject not authorized to message this session",
		}
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*Interact)(nil)
