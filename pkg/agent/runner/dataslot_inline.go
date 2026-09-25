package runner

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// SlotDatum is one of a child's data slots, resolved to the bytes behind it.
type SlotDatum struct {
	// Slot is the name the parent filled — the child's own vocabulary.
	Slot string
	// TagID is the provenance tag governing Content. Carried so a later gate
	// can reason about this datum specifically rather than about the session.
	TagID string
	// Content is the datum. Empty when the tag has no stored content, which is
	// a legitimate state: a tag minted before content storage existed, or from
	// a call whose result was not worth keeping.
	Content string
	MIME    string
}

// ResolveBoundSlotsFor builds the lookup that turns this session's bound data
// slots into the data itself, for placement in its opening context.
//
// # Why the runner resolves rather than a tool
//
// The content lives in pt_tag_content, whose SessionReadable is false: the
// model must not be able to ask for a datum back through query_memory. Placing
// it in the opening context is not a contradiction of that — it is the reason
// the door exists. The PLATFORM decides what the child sees; the child cannot
// widen it by asking.
//
// # Why reading the parent's scope goes through the operator
//
// The bytes were minted in the parent's session, so that is where they live —
// but pt_tag_content is component-READ (SessionReadable false), so this child's
// session credential cannot read the parent scope over the memory API. It asks
// the operator (resolve), which reads the bytes component-side and returns only
// tags the child is ENTITLED to: pt_tag:<id>#access, which a data-slot binding
// grants via granted_to. So the bound set IS the authorization, re-checked by
// the component entitled to make the disclosure — a tag nobody bound onto this
// child is not returned however well-known its id.
//
// A slot whose content is missing is returned with an empty Content rather
// than dropped. The child was told it has that slot; silently omitting it
// would leave the model believing its parent withheld the datum, which is a
// different fact from "the datum was not stored".
func ResolveBoundSlotsFor(
	resolve func(ctx context.Context, scope memory.Scope, tagIDs []string) ([]memory.PtTagContent, error),
	parent memory.Scope,
	list func(ctx context.Context) ([]authz.DataSlotBinding, error),
) func(ctx context.Context) ([]SlotDatum, error) {
	return func(ctx context.Context) ([]SlotDatum, error) {
		if list == nil || resolve == nil {
			return nil, nil
		}
		bindings, err := list(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing this session's bound data slots: %w", err)
		}
		if len(bindings) == 0 {
			return nil, nil
		}
		tagIDs := make([]string, 0, len(bindings))
		for _, b := range bindings {
			tagIDs = append(tagIDs, b.TagID)
		}
		contents, err := resolve(ctx, parent, tagIDs)
		if err != nil {
			// Never a partial context. A child that starts with two of its
			// three slots filled cannot tell that from a parent that filled
			// two, and will act on the shorter set as though it were complete.
			return nil, fmt.Errorf("resolving the data behind this session's slots: %w", err)
		}
		byTag := make(map[string]memory.PtTagContent, len(contents))
		for _, c := range contents {
			byTag[c.TagID] = c
		}

		out := make([]SlotDatum, 0, len(bindings))
		for _, b := range bindings {
			d := SlotDatum{Slot: b.Slot, TagID: b.TagID}
			if c, ok := byTag[b.TagID]; ok {
				d.Content, d.MIME = c.Content, c.MIME
			}
			out = append(out, d)
		}
		return out, nil
	}
}

// SlotContentBlocks renders resolved slots as opening-context blocks.
//
// One block per slot, each naming the slot so the model can tell which is
// which, and each stating that the content came from the delegating agent
// rather than from the model's own work. That framing is load-bearing: a
// datum dropped into context with no provenance reads as something the model
// established itself, and a child that believes it verified a fact it was
// merely handed is the failure this whole path exists to avoid.
//
// The content itself is WRAPPED in the same nonce'd untrusted envelope every
// tool result gets, and the header is deliberately left outside it.
//
// A bound slot is content the parent chose to hand this child, and its origin
// may be a tool result the parent never verified. GradeRequest routes an
// untrusted-origin datum to a human, but that card asks a CONFIDENTIALITY
// question — "should that agent be able to read this" — and approving it binds
// the tag; nothing downstream re-reads carries_untrusted. Delivered as a plain
// user turn it inherited no defense at all: the model's injection rule is
// derived from that envelope's tag, and the content-guard inspectors are
// tool-call hooks that never see an injected turn.
//
// The header stays outside because it is the PLATFORM speaking. Inside, it
// would be attacker-spoofable text sitting in the region the model is told to
// distrust. Each wrap mints its own crypto-random nonce after the content is
// fixed, so a payload carrying an unnonced closing tag cannot end the region
// early.
func SlotContentBlocks(slots []SlotDatum) []memory.ContentBlock {
	blocks := make([]memory.ContentBlock, 0, len(slots))
	for _, s := range slots {
		text := fmt.Sprintf("[input slot %q, provided by the agent that delegated this task %s]\n",
			s.Slot, slotRefMarker(s.TagID))
		if s.Content == "" {
			// The platform's own notice, not delegated content: nothing to
			// distrust, so nothing to wrap.
			text += "(this slot was bound but its content is unavailable; ask for it rather than assuming what it held)"
		} else {
			text += wrapUntrustedToolOutput(s.Content)
		}
		blocks = append(blocks, memory.ContentBlock{Type: "text", Text: text})
	}
	return blocks
}

// slotRefMarker is the reference stamped into a delivered slot's header.
//
// It does double duty, and the second is the load-bearing one: it labels the
// datum for a reader, and it is how a later resume knows this tag has ALREADY
// been delivered. That check reads the transcript, so the marker has to live
// where the transcript keeps it — in the text — rather than in a side table
// that a restart would not restore.
//
// Deliberately terse and machine-shaped. A human-facing card never carries a
// tag id (the decider does not need it and it is noise), but the child already
// holds this tag; naming it costs nothing and buys a delivery record for free.
func slotRefMarker(tagID string) string {
	if tagID == "" {
		return ""
	}
	return "· ref " + tagID
}
