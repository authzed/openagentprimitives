// Package interact is the interaction-kind registry: the generic "what did
// the human do" seam the /interact HTTP endpoint (webd) dispatches to. Each
// Kind declares the SpiceDB permission it requires and how to route the
// submitted payload.
//
// Dependency-light by design: this package imports only channelkinds,
// channelevents, and identity — not pkg/agent/tool — so webd can import it
// as cheaply as pkg/agent/agentcaps, without pulling in the tool graph.
package interact

import (
	"context"
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// Deps is the transport set the /interact endpoint hands every Kind's Submit.
type Deps struct {
	NATSRequest channelevents.RequestFunc
}

// Result is the caller-visible outcome of a Submit call.
type Result struct {
	// Outcome is channelsd's routing verdict for the submission, stringified
	// from channelevents.Outcome.
	Outcome string
	// Notice is the user-facing message to draw, in serialisable form because
	// a Submit caller is an HTTP handler with no category registry to resolve
	// tone from. Nil means there is nothing to show.
	Notice *channelevents.NoticeWire
}

// Kind is a registered interaction kind: a named, permission-gated way for a
// human viewing a session (browser, TUI, …) to act on it.
type Kind interface {
	// Name is the registration key and the wire "kind" value the /interact
	// endpoint dispatches on.
	Name() string

	// Permission is the SpiceDB permission the endpoint must have already
	// checked before calling Submit — "interact" (agentsession#interact) or
	// "approve" (agentsession#approve). Submit itself does not check
	// authorization; it trusts the caller enforced Permission().
	Permission() string

	// ViaSub is the view-URN sub-segment this kind's interactions occupy: ""
	// for a plain message (urn:ap:view:artifact:<id>), or a facet like
	// "annotations". The endpoint mints the server-side Via with this sub, so
	// the kind is recorded structurally in the signed Via rather than sniffed
	// from content — registered here, never branched on in the endpoint.
	ViaSub() string

	// Submit routes the decoded raw payload for session (ns, name) on behalf
	// of the canonical subject, via the server-minted via URN identifying
	// what UI surface originated the interaction.
	Submit(ctx context.Context, deps Deps, ns, name, subject, via string, raw json.RawMessage) (Result, error)
}

// reg is the process-wide interaction-kind registry. Register panics on a
// duplicate or empty name — a programmer error caught at init() time.
var reg = kindregistry.New[Kind]("interact", Kind.Name)

// Register adds k under k.Name(). Called from each kind's init().
func Register(k Kind) { reg.Register(k) }

// Get returns the kind registered under name, and whether one was found.
func Get(name string) (Kind, bool) { return reg.Get(name) }

// Names returns every registered kind name, sorted.
func Names() []string { return reg.Keys() }
