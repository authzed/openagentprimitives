package interact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func init() {
	Register(userMessageKind{})
}

// userMessageKind is the generic "the human typed a message" interaction: free
// text from a view surface, routed to channelsd exactly like a Slack/TUI
// inbound message but carrying a server-minted view Via instead of a
// channel-native thread key.
type userMessageKind struct{}

// userMessagePayload is the wire shape of the raw JSON /interact submits for
// this kind.
type userMessagePayload struct {
	// Text is what the human typed. Whitespace-only is refused, not routed.
	Text string `json:"text"`
}

func (userMessageKind) Name() string { return "user_message" }

// Permission is "interact" (agentsession#interact) — any principal who may
// converse with the session, not just its owner/approver.
func (userMessageKind) Permission() string { return "interact" }

// ViaSub is "" — a plain typed message occupies the bare artifact-view Via
// (urn:ap:view:artifact:<id>), no sub-facet.
func (userMessageKind) ViaSub() string { return "" }

func (userMessageKind) Submit(ctx context.Context, deps Deps, ns, name, subject, via string, raw json.RawMessage) (Result, error) {
	var pl userMessagePayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return Result{}, fmt.Errorf("interact: user_message: decode payload: %w", err)
	}
	text := strings.TrimSpace(pl.Text)
	if text == "" {
		// Refused before any NATS request: an empty submission is a client
		// bug (or a UI double-submit), never a routable message.
		return Result{}, fmt.Errorf("interact: user_message: text is empty")
	}

	// The canonical subject is "user:<base64url(email)>"; decoding recovers the
	// original email, from which channelsd's HandleViewMessage re-derives the
	// SAME canonical id, so this round-trips.
	email := identity.DecodeForDisplay(subject)
	ext := channelkinds.ExternalIdentity{Kind: "idp", Email: identity.Email(email), ExternalID: identity.RawExternalID(email)}

	dec, err := channelkinds.RequestViewMessage(channelkinds.Deps{NATSRequest: deps.NATSRequest}, ns, name, text, via, ext, false, "")
	if err != nil {
		return Result{Outcome: dec.Outcome.String(), Notice: dec.Notice.ToWire()}, fmt.Errorf("interact: user_message: %w", err)
	}
	return Result{Outcome: dec.Outcome.String(), Notice: dec.Notice.ToWire()}, nil
}
