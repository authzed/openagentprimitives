package v1alpha1

import (
	"strconv"
	"time"
)

// AnnotationInboundAttachmentCount records how many attachments rode on the
// inbound message that CREATED this AgentSession. Stamped by the channelsd
// pipeline atomically with the Create; absent means "none".
//
// It exists because a new session's first message arrives SPLIT IN TWO: the
// text goes into spec.Prompt and becomes turn 0, while the attachment note and
// content blocks can only be written afterwards as a separate "inbox" turn —
// composing them needs a fetch/upload/extract round trip that cannot run before
// the session exists to key the upload against, and takes as long as the file
// is big.
//
// The runner is spawned the moment the AgentSession exists, BEFORE that second
// write lands. Without this annotation it cannot tell "no attachments" from
// "attachments not written yet", so it answers a message it has not finished
// receiving. The annotation is the fence: its presence means an inbox turn is
// still owed for turn 0. See Loop.awaitInboundAttachmentTurn.
//
// The decimal count is informational; the fence keys on PRESENCE, because even
// a partially-processed batch writes exactly one inbox turn for the message.
const AnnotationInboundAttachmentCount = "agentprimitives.authzed.com/inbound-attachment-count"

// InboundAttachmentWriteBudget is how long the writer of that inbox turn is
// allowed to take, and therefore exactly how long the runner waits for it
// before giving up and answering without the attachment.
//
// ONE constant deliberately serves both halves. A fence whose budget is chosen
// independently of the work it fences is worse than no fence: shorter than the
// writer's own timeout, it expires on healthy-but-slow batches, silently
// reintroducing the race it exists to close in exactly the case — a big file on
// a slow link — where it matters most.
//
// The value is the channelsd side's whole-batch bound: up to a kind's
// per-message attachment limit (Slack: 10), each fetched serially under its own
// 60s sub-deadline, with extraction capped at 30s inside that.
const InboundAttachmentWriteBudget = 5 * time.Minute

// InboundAttachmentCount reports how many attachments rode on the inbound
// message that created sess, and whether the annotation was present at all.
// A malformed value reads as absent: the annotation is a fence signal, and a
// value nothing wrote deliberately must not hold up the first turn.
func InboundAttachmentCount(ann map[string]string) (count int, present bool) {
	v, ok := ann[AnnotationInboundAttachmentCount]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
