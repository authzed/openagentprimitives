package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// CompletionBypassRecorder builds the func agent_work_complete calls when an
// agent finishes with a completion requirement unmet.
//
// It is one func because the two halves are one fact. Status is the durable
// record an operator reads later; the notice is how the person the round was
// for finds out at the time. Recording only the first would leave the override
// technically logged and practically invisible, which is the shape the bypass
// exists to avoid.
//
// Both halves must land, and a failure in either fails the whole call: the tool
// refuses a bypass it could not record, so the agent is told rather than
// quietly getting what it asked for. pub may be nil for a session with no
// channel (kubectl-driven), where status IS the only surface there is and the
// absence is logged rather than treated as a failure.
//
// signer signs the notice's envelope with the session's identity key.
// Nil-safe: a nil signer publishes unsigned (test fixtures without a signer
// still work).
func CompletionBypassRecorder(
	sp *StatusPatcher,
	pub channelevents.PublishFunc,
	signer *channelevents.EnvelopeSigner,
	sess channelevents.SessionRef,
	logf func(msg string, keysAndValues ...any),
) func(ctx context.Context, b completion.Bypass) error {
	return func(ctx context.Context, b completion.Bypass) error {
		if sp == nil {
			return errors.New("no status patcher wired, so a completion bypass cannot be recorded")
		}
		ordinal, err := sp.RecordCompletionBypass(ctx, b)
		if err != nil {
			return fmt.Errorf("record completion bypass on session status: %w", err)
		}
		if pub == nil {
			// Not a failure: a session with no channel has no second surface,
			// and status already holds the record. Loud, though — "the user was
			// never told" must never be something only the absence of a message
			// reveals.
			if logf != nil {
				logf("completion bypass recorded on status only; this session has no channel to tell anyone on",
					"session", sess.Namespace+"/"+sess.Name, "requirements", keysOf(b.Unmet))
			}
			return nil
		}
		// The ref names THIS bypass, so two overrides in one session are two
		// distinct requests on the wire rather than one repeated ref. Built from
		// the durable ordinal the status write just returned.
		ref := fmt.Sprintf("notice-completion-bypassed-%s-%d", sess.Name, ordinal)
		if err := bypassNotice(b).PublishSigned(signer, pub, sess, ref); err != nil {
			return fmt.Errorf("publish completion-bypass notice: %w", err)
		}
		return nil
	}
}

// bypassNotice is the user-facing message. Split out so what a person reads is
// one readable function rather than a literal buried in wiring.
//
// The agent's Reason travels in Excerpt, never in Lead/Body/NextStep: it is
// agent-authored text, and the trusted fields are rendered as live markup by
// every surface. What was skipped is stated by the runtime, in the requirement
// Titles — which is why Title is user copy and Missing (which names render
// handles and step ids) is not.
func bypassNotice(b completion.Bypass) *notice.Notice {
	titles := make([]string, 0, len(b.Unmet))
	for _, u := range b.Unmet {
		titles = append(titles, u.Title)
	}
	body := "This agent is set up to make sure " + humanList(titles) +
		" before it calls a round finished. It finished this one anyway, and gave a reason."

	return notice.New(categories.CompletionRequirementBypassed, notice.Args{
		Lead:     "The agent finished without everything it was set up to deliver",
		Body:     body,
		NextStep: "Read the agent's reason below and, if you still need what was skipped, ask for it in this thread.",
		Excerpt: &channelevents.InteractionExcerpt{
			Label:   "The agent's reason",
			Content: b.Reason,
		},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// humanList renders a title list as "a", "a and b", or "a, b and c".
func humanList(items []string) string {
	switch len(items) {
	case 0:
		return "everything it promised"
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// keysOf is the requirement-key list, for logs and for the status record.
func keysOf(unmet []completion.Unmet) []string {
	out := make([]string, 0, len(unmet))
	for _, u := range unmet {
		out = append(out, u.Key)
	}
	return out
}
