package interact

import (
	"context"

	"github.com/go-logr/logr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

// Deps is the /interact plugin's dependency interface. internal/cmd/webd's concrete
// deps value implements it; the WebUI casts webui.Deps to this in Routes(). A
// cast failure (nil, or a webd umbrella missing one of these collaborators)
// means the interact routes fail closed — see interact.go's Routes.
type Deps interface {
	// CheckInteract is the send-authorization check: agentsession#interact for
	// (ns, name) on behalf of subject. This is the ONLY permission the POST
	// handler gates a send on — never CheckArtifactView, which is a strict
	// superset (parent->interact + parent->artifact_org_view +
	// platform->view_audit) that would admit platform admins — and, on an
	// org-visible session, any authenticated user — into sending as a
	// participant.
	CheckInteract(ctx context.Context, ns, name, subject string) (bool, error)
	// CheckArtifactView confirms subject holds artifact#view on artifactID.
	// Used ONLY as one half of validating an artifact before minting its Via
	// — never as the send authorization. It is a GLOBAL permission check on
	// the artifact: it says nothing about which session the artifact lives
	// in, so it must always be paired with ListRevisions below.
	CheckArtifactView(ctx context.Context, artifactID, subject string) (bool, error)
	// ListRevisions is the artifact→session BINDING PROBE, not a listing the
	// handler wants the result of. The read is scoped strictly to
	// memory.Scope{Kind:"session", ID: ns+"/"+sess} and hard-errors when the id
	// is not in that scope, so a successful call is proof the artifact lives in
	// THAT session. Artifact ids are globally unique, so at most one session
	// can survive the probe and it is the artifact's own.
	//
	// The same probe pkg/web/webui/artifactview's bindView runs, with a
	// signature matching artifactview.Deps.ListRevisions verbatim: webd's
	// single umbrella implements both interfaces and Routes() casts at RUNTIME,
	// so a method shape webd does not already satisfy would compile fine and
	// silently drop every interact route.
	ListRevisions(ctx context.Context, ns, sess, artifactID string) ([]artifactview.RevisionMeta, error)
	// AgentClassOf resolves the AgentClass backing session (ns, name), so the
	// handler can read its session_views capability grant. An error here means
	// the class can't be resolved and the request fails closed (no
	// session_views ⇒ no interaction is permitted).
	AgentClassOf(ctx context.Context, ns, name string) (*spiceboxv1alpha1.AgentClass, error)
	// WidgetOriginOf resolves the MCPServer origin recorded on this session's
	// status.activeWidgets entry for artifactID — the identity of the widget
	// making an app-tool call.
	//
	// It exists so the ANSWER comes from the session's own durable status
	// rather than from the browser: the shell says WHICH widget is calling (an
	// artifact id it already has), and the server says what that widget IS. A
	// widget naming another widget's artifact id therefore pins itself to that
	// other origin, which is stricter, never laxer.
	//
	// found=false means the session has no such widget. The caller refuses the
	// call: an artifact id that resolves to nothing is either a stale widget or
	// a forged one, and neither may fall back to an unpinned call.
	WidgetOriginOf(ctx context.Context, ns, name, artifactID string) (origin string, found bool, err error)
	// NATSRequest is the request/reply transport handed to the interaction
	// kind's Submit (pkg/channels/interact.Deps.NATSRequest).
	NATSRequest() channelevents.RequestFunc
	// TrustedOrigin returns the live trusted-origin base URL, used to pin the
	// POST's Origin header (CSRF).
	TrustedOrigin() string
	Logger() logr.Logger
}
