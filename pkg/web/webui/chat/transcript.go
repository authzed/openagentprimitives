package chat

import (
	"context"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/web/webui/livemirror"
)

// ErrNoMemoryAccess is returned by readTranscript when webd was started without
// a memory token (or operator URL) — the transcript endpoint fails closed with
// a clear message rather than silently returning an empty conversation.
var ErrNoMemoryAccess = livemirror.ErrNoMemoryAccess

// timelineEntry is one ordered item of a resumed conversation: a message or a
// plan card. Mirrors the client's TimelineItem (types.ts); an alias, so chat
// and the shared history reader stay a single definition.
type timelineEntry = livemirror.TimelineEntry

// readTranscript reads a chat session's durable transcript turns from operator
// memory (via webd's read-only memory token) and maps them to the ordered
// timeline — messages AND plan cards — a resumed conversation renders.
//
// The mapping itself lives in the session-agnostic livemirror.ReadHistory, so
// a session-view page replays history identically; this wrapper supplies
// chat's Deps-sourced operator URL, token, and logger, and fills in the
// opening turn the runner has not placed yet (openingTurn).
func readTranscript(ctx context.Context, d Deps, ns, name string) ([]timelineEntry, error) {
	h, err := livemirror.ReadHistory(ctx, d.OperatorURL(), d.MemoryToken(), ns, name, d.Logger())
	if err != nil {
		return nil, err
	}
	if d.K8s() == nil {
		d.Logger().Info("chat: session metadata unavailable; replaying the durable transcript", "session", ns+"/"+name)
		return h.Timeline, nil
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		d.Logger().Info("chat: get AgentSession for the opening turn failed; replaying without it",
			"session", ns+"/"+name, "err", err.Error())
		return h.Timeline, nil
	}
	if sess.Spec.OpeningSummary != "" && sess.Spec.Prompt.Inline != "" {
		// The operator's execution instructions remain in the durable audit
		// transcript. Replace only their opening bubble in the conversation;
		// subsequent human messages, even identical ones, remain visible.
		items := h.Timeline
		if h.HasOpeningTurn {
			for i, item := range items {
				if item.Kind == "message" && item.Role == turn.VisibleRoleUser && item.Text == strings.TrimSpace(sess.Spec.Prompt.Inline) {
					items = append(items[:i:i], items[i+1:]...)
					break
				}
			}
		}
		return append([]timelineEntry{{Kind: "opening", Opening: &channelevents.SessionOpening{
			Summary: sess.Spec.OpeningSummary, Instructions: sess.Spec.Prompt.Inline,
		}, CreatedAt: sess.CreationTimestamp.Time}}, items...), nil
	}
	if h.HasOpeningTurn {
		return h.Timeline, nil
	}
	opening := openingTurn(&sess)
	if len(opening) == 0 {
		// Returned as-is rather than through append, which would turn a
		// legitimately empty timeline into a nil one — "timeline": null on the
		// wire where every other path sends [].
		return h.Timeline, nil
	}
	return append(opening, h.Timeline...), nil
}

// openingTurn returns the session's opening message as a one-entry timeline to
// prepend, or nothing.
//
// It is called ONLY when the durable transcript has no turn 0, which is the
// whole window between a session being created and its runner appending its
// copy of spec.prompt.inline — seconds, and exactly the window a viewer who
// just pressed "start" spends looking at this transcript. The dashboard's start
// route navigates with a full page load, so the message they typed is gone from
// the browser; nothing republishes it (user_echo carries only view-originated
// messages); and the mount-time read never repeats. Without this, the viewer's
// own first message was missing until they reloaded the page by hand.
//
// spec.prompt.inline is the same text the runner will place as turn 0, so the
// entry this returns is replaced by an identical one rather than doubled once
// the runner catches up. Only inline prompts are read — a configMapRef prompt
// would need a second read of an object this route has no business fetching,
// and no browser-started session uses one.
//
// Best-effort by design: a session whose object is gone still replays its
// durable transcript (the point of authorizeRead), so a failed Get is logged
// and yields nothing rather than failing the whole conversation.
func openingTurn(sess *spiceboxv1alpha1.AgentSession) []timelineEntry {
	text := strings.TrimSpace(sess.Spec.Prompt.Inline)
	if text == "" {
		return nil
	}
	return []timelineEntry{{
		Kind: "message", Role: turn.VisibleRoleUser, Text: text,
		// The session's own creation time: the moment the viewer sent it, and
		// early enough that the entry sorts ahead of everything the agent says.
		CreatedAt: sess.CreationTimestamp.Time,
	}}
}
