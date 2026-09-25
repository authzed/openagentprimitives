package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// MaxPersistentNativeBlocks bounds how many attachments with no text
// fallback stay attached as native blocks across a session. A full-size
// screenshot costs roughly 1,500-1,600 input tokens, so four is about 6k
// tokens on every request — material but not dominant. Tunable; nothing
// depends on the exact value.
const MaxPersistentNativeBlocks = 4

// maxNativeBytesPerRequest bounds the total native-block payload in one
// request. Anthropic's ceiling is 32 MB for the whole request; the margin
// leaves room for conversation text, tool definitions, and base64's ~33%
// expansion. Exceeding the real ceiling fails the turn outright, so this is
// deliberately conservative.
//
// It is also the only bound on the head turn, which the newest-N window does
// not cover: a turn may arrive with any number of text-fallback attachments.
const maxNativeBytesPerRequest = 20 << 20

// hydrateAttachments resolves every attachment reference in msgs into its
// final form for ONE provider call, returning this request's view: a shallow
// copy whose changed messages carry freshly built Content slices.
//
// It never mutates msgs. That slice is the loop's long-lived, append-only
// conversation, so hydrating in place would retype the block away from
// "attachment" and hide it from every later pass. Both bounds below count what
// is ATTACHED, not what one pass rendered, so both would silently stop
// enforcing — an image thread would gain a native block per turn until the
// provider rejected the request. In-place would also rewrite an EARLY message
// late in a session, defeating the prompt cache.
//
// It is a whole-slice pass because three policies need a whole-conversation
// view: which turn is the head, which attachments fall inside the newest-N
// window, and whether the request still fits the per-request byte ceiling.
// Deriving all three per request is why the window can never drift from the
// turn record — nothing about it is stored.
//
// A ref becomes a native block only if:
//
//  1. the model declares the MIME natively (l.Provider.NativeInputMIMEs) AND
//     the declared block type is non-empty. That type comes from the registry,
//     never from parsing the MIME string — see llm.MIMESet.
//  2. lifetime follows the text fallback, not the MIME family: with a TextRef
//     the block lives on its arrival ("head") turn only, since the extracted
//     text represents it afterward; without one it persists across the
//     newest-N window. "Images persist" falls out of images having no
//     extractor — it is not coded as a special case.
//
// POST-CONDITION 1: no "attachment" block survives — provider adapters
// hard-error on unknown block types. Hence the unconditional clear at the end
// rather than per-branch clearing; nativeBlockFor guards the same error's
// other door, a native block built with an empty Type.
//
// POST-CONDITION 2: a message that had content still has content.
// contentBlocksFromMemory keeps an all-unrenderable turn alive by passing its
// attachment block through as a ref — but that runs before this pass removes
// that very block, so a turn whose only content was one ref would leave empty.
// An empty Content slice is its own fatal provider error, distinct from an
// unknown block type, so this check is NOT redundant with that fallback: it
// covers exactly the case the fallback can no longer see.
func (l *Loop) hydrateAttachments(ctx context.Context, msgs []llm.Message) []llm.Message {
	// out is this request's view. Copying the message structs keeps the
	// caller's slice pristine: every write below is either to out[i] itself or
	// to a Content slice allocated here. An unchanged message keeps sharing the
	// caller's blocks — the conversation is re-walked on every request, so
	// copying content nobody rewrote would be pure garbage.
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)

	native := llm.MIMESet(nil)
	if l.Provider != nil {
		native = l.Provider.NativeInputMIMEs(l.Model)
	}
	// A session whose provider rejected a native block behaves from here on as
	// though the model declared no native MIMEs. Resolving suppression to an
	// empty set rather than branching later keeps it one decision: every
	// attachment then takes the existing "this model cannot take the type"
	// path, which already declines to build a block AND writes the head-turn
	// note. A parallel suppression branch would have to re-derive both, and
	// would be the obvious place for the two to drift.
	if l.nativeSuppressed.Load() {
		native = nil
	}

	// headTurn is the boundary the TextRef!="" rule expires against: the most
	// recent message that represents genuine user input.
	//
	// NOT "the last message in msgs": the tool-calling loop appends an
	// assistant tool_use message and then a Role:"user" message the runner
	// synthesizes to relay the tool_result back to the provider (an Anthropic
	// API mechanic, not a human speaking). Treating that as the head turn
	// would expire the attachment mid-tool-loop, on a turn the agent may still
	// need the document for.
	headTurn := mostRecentUserTurn(msgs)

	// Pass 1: locate every ref and decide eligibility.
	type site struct{ msg, block int }
	var sites []site
	for i := range msgs {
		for j, b := range msgs[i].Content {
			if b.Type == "attachment" && b.Attachment != nil {
				sites = append(sites, site{i, j})
			}
		}
	}

	// Persistent candidates are those with no text fallback, newest first;
	// only the newest MaxPersistentNativeBlocks stay attached.
	//
	// Two buckets collect the attachments nothing left in the request
	// represents, each getting its own head-turn note at the end of this pass.
	// An attachment WITH a text fallback is in neither — its manifest line and
	// fetch_artifact handle still describe it.
	//
	//   - outOfView: the model CAN take this type, but the newest-N window or
	//     the byte budget dropped it. "You could see this; you no longer can."
	//   - unreadable: the contents never reached the agent at all — this model
	//     does not take the type natively, or the bytes could not be read.
	persistentBudget := MaxPersistentNativeBlocks
	keep := make(map[site]bool, len(sites))
	outOfView := make(map[site]bool)
	unreadable := make(map[site]bool)
	for k := len(sites) - 1; k >= 0; k-- {
		s := sites[k]
		ref := msgs[s.msg].Content[s.block].Attachment
		if ref.ArchiveRef != "" && !l.isPinned(ref.Ref) {
			// An archive member. Its archive's INDEX already named it and stated
			// its readability, so it needs no native block and no note — and
			// must not take a window slot from the file the user actually sent
			// alongside it.
			//
			// Without this, a 187-file support bundle fills the newest-N window
			// with four arbitrary log files, fails nativeBlockFor on their MIME,
			// and names them all in appendUnreadableNote — files the agent was
			// never told about in the first place.
			//
			// Pinning is the deliberate route back into view, which is exactly
			// why members still get blocks: PinAttachment validates the handle
			// against the session's own turns and would reject one otherwise.
			continue
		}
		if !native.Has(ref.MIME) {
			// This model cannot take the type natively. With extracted text
			// that is harmless — ingestion's manifest line names the
			// fetch_artifact handle the agent reads the file through.
			//
			// With NO extracted text nothing represents the file: ingestion's
			// line is the neutral "<name> (<type>) was attached." — no content,
			// no handle, and no statement that it cannot be read, since the
			// resolved model is unknown at ingestion time. Only this pass knows,
			// so without the note the agent has a filename and every reason to
			// answer about a file it has never seen.
			if ref.TextRef == "" {
				unreadable[s] = true
			}
			continue
		}
		switch {
		case l.isPinned(ref.Ref):
			// The agent asked for this one back by name (show_attachment); an
			// explicit request outranks the recency guess the window encodes.
			// Deliberately does NOT consume persistentBudget — a pin ADDS a
			// file, and spending window slots on pins would silently evict the
			// recent attachments the agent is probably comparing against.
			// Pass 2's per-request byte budget is what keeps this bounded.
			keep[s] = true
		case ref.TextRef == "":
			if persistentBudget > 0 {
				persistentBudget--
				keep[s] = true
			} else {
				outOfView[s] = true
			}
		case s.msg == headTurn:
			keep[s] = true
		}
	}

	// Pass 2: fetch the bytes for everything pass 1 kept, newest first, and
	// spend the per-request byte budget in that order. Blowing the provider's
	// ceiling fails the entire turn, so something must give — and the file the
	// user just sent is the one the turn is probably about. Fetching here
	// rather than during assembly is what makes newest-first possible:
	// assembly walks messages oldest-first, which would hand the budget to
	// whichever attachment happens to be earliest in the conversation.
	blocks := make(map[site]llm.ContentBlock, len(keep))
	remaining := maxNativeBytesPerRequest
	for k := len(sites) - 1; k >= 0; k-- {
		s := sites[k]
		if !keep[s] {
			continue
		}
		ref := msgs[s.msg].Content[s.block].Attachment
		blk, ok := l.nativeBlockFor(ctx, native, ref)
		if !ok {
			// Already logged by nativeBlockFor, with the reason it degraded.
			// The agent's side of it is the same class as the undeclared-MIME
			// case in pass 1: with no extracted text, nothing left in the
			// request represents this file, and only this pass knows that.
			if ref.TextRef == "" {
				unreadable[s] = true
			}
			continue
		}
		if len(blk.Data) > remaining {
			slog.Default().Info("attachment hydration: would exceed the per-request native-byte budget; falling back to reference form",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"handle", ref.Ref, "mime", ref.MIME,
				"bytes", len(blk.Data), "remainingBytes", remaining, "budgetBytes", maxNativeBytesPerRequest)
			if ref.TextRef == "" {
				outOfView[s] = true
			}
			continue
		}
		remaining -= len(blk.Data)
		blocks[s] = blk
	}

	// Pass 3: bracket every fetched block with untrusted markers, rebuild each
	// affected message's Content as a FRESH slice, note what dropped out of
	// view, then clear every remaining ref.
	//
	// A native block sits in a user-role message — the most trusted position in
	// the conversation — so without bracketing, a PDF or screenshot saying
	// "ignore previous instructions" arrives indistinguishable from the user's
	// own typed words. Binary content can't be wrapped inline the way
	// wrapUntrustedToolOutput wraps a text tool result, so the markers are
	// sibling text blocks either side, sharing the same nonce discipline
	// (newUntrustedOutputNonce, loop.go) as wrapper and prompt rule. The nonce
	// is per-attachment and reused for the session (attachmentNonce): a matched
	// pair bounds exactly one region, and reuse keeps an attachment's rendering
	// byte-identical turn to turn.
	//
	// Rendering a site turns ONE block into THREE, which would invalidate every
	// later site{msg, block} index within the same message — pass 1's indices
	// were recorded against the ORIGINAL block positions. So this walks each
	// affected message's ORIGINAL content slice, consults `blocks` by original
	// index, and assigns the rebuilt slice only after the whole message is
	// walked.
	rebuild := make(map[int]bool, len(blocks))
	for s := range blocks {
		rebuild[s.msg] = true
	}
	for i := range msgs {
		if !rebuild[i] {
			continue
		}
		orig := msgs[i].Content
		built := make([]llm.ContentBlock, 0, len(orig)+2)
		for j, b := range orig {
			blk, ok := blocks[site{i, j}]
			if !ok {
				// Not rendered — ineligible, unfetchable, or over budget. It
				// stays in reference form; clearRemainingRefs strips it below.
				built = append(built, b)
				continue
			}
			ref := b.Attachment
			nonce := l.attachmentNonce(ref.Ref)
			built = append(built,
				llm.ContentBlock{Type: "text", Text: fmt.Sprintf("<%s nonce=%q filename=%q mime=%q>",
					untrusted.AttachmentTag, nonce, ref.Filename, ref.MIME)},
				blk,
				llm.ContentBlock{Type: "text", Text: fmt.Sprintf("</%s nonce=%q>", untrusted.AttachmentTag, nonce)},
			)
		}
		out[i].Content = built
	}
	// Name both buckets in conversation order (oldest first). The switch keeps
	// one file out of both notes: the buckets are disjoint by construction
	// today, and a future edit that overlapped them would otherwise announce
	// the same file twice, contradictorily, in the same request.
	var outOfViewNames, unreadableNames []string
	for _, s := range sites {
		ref := msgs[s.msg].Content[s.block].Attachment
		switch {
		case outOfView[s]:
			// The out-of-view note carries the HANDLE alongside the name,
			// because it is the one note the agent can act on: show_attachment
			// takes a handle, and a note that named only the file would be
			// telling the agent to call a tool while withholding its argument.
			outOfViewNames = appendUniqueName(outOfViewNames,
				fmt.Sprintf("%s (handle %s)", strconv.Quote(ref.Filename), ref.Ref))
		case unreadable[s]:
			unreadableNames = appendUniqueName(unreadableNames, ref.Filename)
		}
	}
	l.appendOutOfViewNote(out, outOfViewNames, headTurn)
	l.appendUnreadableNote(out, unreadableNames, headTurn)
	l.clearRemainingRefs(out)
	return out
}

// appendOutOfViewNote appends one text block naming the attachments that WERE
// eligible to be shown natively and are no longer in the request: dropped by
// the newest-N window or the byte budget, with no text fallback to represent
// them. Nothing else in the conversation says so, and the agent would otherwise
// answer about a file it can no longer see — a confident wrong answer rather
// than a provider error.
//
// It lands on the head turn (appendHeadTurnNote), whose position moves as the
// conversation grows; anywhere earlier would re-cost more of the cached prefix.
// Derived from the refs on every pass and written only into the request copy,
// so it cannot accumulate.
func (l *Loop) appendOutOfViewNote(msgs []llm.Message, names []string, headTurn int) {
	if len(names) == 0 {
		return
	}
	if !l.headTurnCanCarryNote(msgs, headTurn, "no-longer-visible", names) {
		return
	}
	slog.Default().Info("attachment hydration: attachments no longer in view; telling the agent",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
		"count", len(names), "files", strings.Join(names, ","))

	// Names already arrive rendered as `"file" (handle mem://…)` — see the
	// bucket-collection loop — so they are not re-quoted here.
	note := fmt.Sprintf("[No longer visible in this conversation: %s. Only the %d most recent attachments without extracted text stay attached, "+
		"and an attachment is also dropped when the request would otherwise exceed the model's size limit. "+
		"If you need one of these again, call show_attachment with its handle and it will be visible from your next message; "+
		"otherwise carry on without it. Do not describe a file you can no longer see.]",
		strings.Join(names, ", "), MaxPersistentNativeBlocks)

	appendHeadTurnNote(msgs, headTurn, note)
}

// appendUnreadableNote appends one text block naming the attachments whose
// CONTENTS never reached this request at all: no native block was ever built
// for them, and no text fallback stands in. Its sibling appendOutOfViewNote
// covers files the agent could once see; this one covers files it never could.
//
// Without it the agent's entire signal is ingestion's neutral "<name> (<type>)
// was attached." — no content, no handle, no statement that the file cannot be
// read — which invites a confident answer about a file nobody showed the model.
// Ingestion cannot say it instead: readability depends on the model resolved at
// send time.
//
// The two causes — the model does not take the type natively, and the stored
// bytes could not be read — are stated together because the agent's move is
// identical either way, and naming the wrong one would falsely imply that
// re-sending the same file could work.
func (l *Loop) appendUnreadableNote(msgs []llm.Message, names []string, headTurn int) {
	if len(names) == 0 {
		return
	}
	if !l.headTurnCanCarryNote(msgs, headTurn, "not-readable", names) {
		return
	}
	slog.Default().Info("attachment hydration: attachments whose contents never reached the model; telling the agent",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
		"count", len(names), "files", strings.Join(names, ","))

	note := fmt.Sprintf("[Attached but not readable: %s. %s reached this conversation, but the contents are not available to you — "+
		"either this model cannot take that kind of file directly, or the stored file could not be read. "+
		"Do not guess at what it contains: say you cannot see it, and ask the user to paste the relevant part as text or re-send it in another form.]",
		strings.Join(quotedNames(names), ", "), pluralFileSubject(len(names)))

	appendHeadTurnNote(msgs, headTurn, note)
}

// headTurnCanCarryNote reports whether headTurn indexes a message an
// agent-facing note may be appended to, logging when it does not. Both notes
// are the agent's ONLY signal about the files they name, so a note with nowhere
// to land goes to the log rather than vanishing or being guessed onto some
// other message. Reachable only if attachments arrived on no human-authored
// turn.
func (l *Loop) headTurnCanCarryNote(msgs []llm.Message, headTurn int, kind string, names []string) bool {
	if headTurn >= 0 && headTurn < len(msgs) {
		return true
	}
	slog.Default().Info("attachment hydration: attachments need a note but the request has no user turn to put it on",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
		"note", kind, "files", strings.Join(names, ","))
	return false
}

// appendHeadTurnNote appends note as a text block on the head turn — the newest
// human-authored message, which is the turn the agent is answering and the
// latest position in the request a note can occupy.
//
// A fresh slice, never append-in-place: msgs is the request copy, whose
// unchanged messages still share the CALLER's blocks, so appending in place
// could write straight through into the loop's long-lived conversation.
func appendHeadTurnNote(msgs []llm.Message, headTurn int, note string) {
	orig := msgs[headTurn].Content
	withNote := make([]llm.ContentBlock, len(orig), len(orig)+1)
	copy(withNote, orig)
	msgs[headTurn].Content = append(withNote, llm.ContentBlock{Type: "text", Text: note})
}

// quotedNames quotes each filename for inclusion in a note. Quoted, not bare:
// a filename is user-supplied text landing in a trusted position, and quoting
// keeps it from spanning lines or forging a marker of its own.
func quotedNames(names []string) []string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	return quoted
}

// pluralFileSubject renders the sentence subject that follows a list of
// filenames, so the note reads as English for one file and for several.
func pluralFileSubject(n int) string {
	if n == 1 {
		return "It"
	}
	return "They"
}

// mostRecentUserTurn returns the index of the highest-index message in msgs
// that represents genuine user input, or -1 if none does. NOT len(msgs)-1 nor
// the last Role:"user" message: a Role:"user" message carrying only tool_result
// blocks is the runner's own synthesized relay of tool output back to the
// provider and must not move the boundary. See hydrateAttachments' headTurn
// comment.
func mostRecentUserTurn(msgs []llm.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if isToolResultRelay(msgs[i]) {
			continue
		}
		return i
	}
	return -1
}

// isToolResultRelay reports whether m is a Role:"user" message whose content is
// exclusively tool_result blocks — the shape loop.go appends after dispatching
// tool calls (`messages = append(messages, llm.Message{Role: "user", Content:
// toolResultBlocks})`), never a message a human sent. An empty-content message
// is NOT a relay: never observed in practice, and the conservative default is
// to let it count as a user-turn boundary rather than silently extend an
// attachment's lifetime past it.
//
// LANDMINE for whoever wires tool.Result.ContainerUpload (documented "NOT YET
// CONSUMED" on the field): put the container_upload block in its OWN message.
// Merging a non-tool_result block into the relay makes this function stop
// recognizing it, so mostRecentUserTurn moves the head-turn boundary onto the
// runner's own synthesized message and every attachment with extracted text
// expires MID-TOOL-LOOP. Nothing fails; the agent just quietly stops seeing
// the document it was sent.
func isToolResultRelay(m llm.Message) bool {
	if len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// attachmentNonce returns the untrusted-marker nonce for the attachment at
// handle, minting one on first use and reusing it for the rest of the session.
// Two different handles never share a nonce (a matched pair bounds exactly one
// untrusted region), and one handle's nonce never changes — hydration rebuilds
// native blocks from their refs on every request, so a per-call nonce would
// rewrite every attached block's markers each turn and re-cost the cached
// prefix from the earliest one onward.
//
// Reuse is safe because a nonce only has to be unforgeable BY THE FILE IT
// WRAPS, and the bytes were frozen at upload, before the nonce existed. An
// empty handle gets a fresh nonce and no memo entry: two handle-less
// attachments are not the same attachment.
func (l *Loop) attachmentNonce(handle string) string {
	if handle == "" {
		return newUntrustedOutputNonce()
	}
	l.attachmentNoncesMu.Lock()
	defer l.attachmentNoncesMu.Unlock()
	if n, ok := l.attachmentNonces[handle]; ok {
		return n
	}
	n := newUntrustedOutputNonce()
	if l.attachmentNonces == nil {
		// Loop is built as a bare struct literal at every call site, so the
		// map is allocated on first write rather than at construction.
		l.attachmentNonces = make(map[string]string)
	}
	l.attachmentNonces[handle] = n
	return n
}

// nativeBlockFor reads an attachment's bytes and builds its native block.
// ok=false means "render it as a reference instead" — every failure here is
// non-fatal by design: a turn that drops one attachment beats a turn that
// fails. l.ArtifactReader may be nil (kubectl-driven sessions with no artifact
// store); that degrades every attachment rather than panicking.
//
// STANDING RULE: every failure returns ok=false, and ok=true requires bytes
// that were positively validated — a successful read is not validation, since
// a read can succeed and return nothing. An attachment with no TextRef lives in
// the PERSISTENT window, so it is rebuilt into every subsequent request: a
// block the provider rejects costs the session, not one turn. The 400 fails the
// turn, the retry rebuilds the identical block, the retry budget drains, and
// the durable memory turn replays it after a restart.
func (l *Loop) nativeBlockFor(ctx context.Context, native llm.MIMESet, ref *llm.AttachmentRef) (llm.ContentBlock, bool) {
	// Resolve the block type FIRST. native.Has(ref.MIME) only proves the key is
	// present — NewMIMESet does no validation, so a model row mapping a MIME to
	// "" (a one-character typo in exactly the "adding a format is a one-row
	// edit" workflow, e.g. {"image/heic": ""}) would otherwise reach the
	// ArtifactReader and produce llm.ContentBlock{Type: ""}. That type isn't
	// "attachment", so clearRemainingRefs would not strip it and the provider
	// adapter would reject it as `unknown content block type ""` — precisely
	// the failure post-condition 1 exists to prevent.
	bt := native.BlockType(ref.MIME)
	if bt == "" {
		slog.Default().Info("attachment hydration: registry declared MIME with no block type; falling back to reference form",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"handle", ref.Ref, "mime", ref.MIME)
		return llm.ContentBlock{}, false
	}
	if l.ArtifactReader == nil {
		return llm.ContentBlock{}, false
	}
	data, _, err := l.ArtifactReader.ReadRange(ctx, artifactstore.Ref(ref.Ref), 0, 0)
	if err != nil {
		slog.Default().Info("attachment hydration: read failed; falling back to reference form",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"handle", ref.Ref, "mime", ref.MIME, "err", err.Error())
		return llm.ContentBlock{}, false
	}
	if len(data) == 0 {
		// A read that succeeds and returns nothing is still a failure, and
		// degrades the same way. The upload path enforces a maximum but no
		// minimum and does not sniff content (memory.AttachmentBlock.MIME is a
		// hint, not a verified fact), so a 0-byte upload reaches here with a
		// plausible MIME. Emitted as a native block it becomes a provider 400 on
		// EVERY request that rebuilds it (no TextRef ⇒ persistent window), which
		// the retry path cannot clear — see this function's standing rule.
		slog.Default().Info("attachment hydration: read returned zero bytes; falling back to reference form",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"handle", ref.Ref, "mime", ref.MIME)
		return llm.ContentBlock{}, false
	}
	// The block type comes from the registry, never from inspecting the MIME.
	// A strings.HasPrefix(ref.MIME, "image/") here would be a second opinion
	// the model table could not override — see MIMESet's doc comment.
	return llm.ContentBlock{Type: bt, MIME: ref.MIME, Data: data}, true
}

// clearRemainingRefs enforces post-condition 1: no attachment ref reaches a
// provider adapter. Refs rendered natively are already gone (pass 3 rebuilt
// their message's Content without the original ref block); this removes the
// rest, which ingestion's sibling manifest line already describes to the agent.
//
// It also re-establishes post-condition 2: a message whose only surviving
// content was the ref just removed gets the empty-content placeholder that
// contentBlocksFromMemory's own guard can no longer reach, since that guard
// runs before this pass resolves "attachment" into a native block or nothing.
//
// msgs is the request-scoped copy, whose untouched messages still share the
// CALLER's blocks — so a message that needs clearing gets a freshly allocated
// slice rather than being compacted in place, which would write straight
// through into the loop's long-lived conversation.
func (l *Loop) clearRemainingRefs(msgs []llm.Message) {
	for i := range msgs {
		if !hasRefShapedBlock(msgs[i].Content) {
			continue
		}
		orig := msgs[i].Content
		kept := make([]llm.ContentBlock, 0, len(orig))
		for _, b := range orig {
			if b.Type == "attachment" {
				continue
			}
			// Defensive, not currently load-bearing: no code path today sets
			// Attachment on a block whose Type isn't "attachment" (native
			// blocks are built fresh, with no Attachment field set). Clearing
			// it unconditionally means a future bug that DID leave it set
			// still can't leak a ref-shaped value past this pass. b is a copy,
			// so this never reaches the caller's block.
			b.Attachment = nil
			kept = append(kept, b)
		}
		if len(orig) > 0 && len(kept) == 0 {
			kept = append(kept, llm.ContentBlock{Type: "text", Text: emptyContentPlaceholderText})
		}
		msgs[i].Content = kept
	}
}

// hasRefShapedBlock reports whether any block would be rewritten by
// clearRemainingRefs — either an attachment ref to strip or a stray
// Attachment field to clear. It is what lets the clearing pass skip
// (and therefore keep sharing) every message it would not have changed.
func hasRefShapedBlock(blocks []llm.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "attachment" || b.Attachment != nil {
			return true
		}
	}
	return false
}

// requestHasNativeBlock reports whether req carries any native attachment
// block. It is the trigger for the one-shot suppression retry: only a request
// that actually contained one can have been rejected because of one, so a
// session that never attached a file pays nothing for this path.
func requestHasNativeBlock(req llm.Request) bool {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.NativeBlockImage || b.Type == llm.NativeBlockDocument {
				return true
			}
		}
	}
	return false
}

// appendUniqueName appends name unless already present, preserving order.
// Filenames are user-supplied and routinely collide — two pasted screenshots
// are both "image.png" — and a note reading `"image.png", "image.png"` reads to
// the agent as two distinct files. Order matters (the notes are oldest-first),
// so this is a linear scan rather than a set; the lists are bounded by how many
// attachments one session holds.
func appendUniqueName(names []string, name string) []string {
	for _, n := range names {
		if n == name {
			return names
		}
	}
	return append(names, name)
}

// ErrAttachmentNotInSession reports that a handle passed to PinAttachment
// does not name an attachment this session ever received.
var ErrAttachmentNotInSession = errors.New("no attachment with that handle in this session")

// PinAttachment marks an attachment to be shown again on the next request,
// exempting it from the newest-N window that let it fall out of view. It backs
// the show_attachment tool: an attachment with no extracted text is otherwise
// unrecoverable once evicted, since fetch_artifact returns raw bytes a model
// cannot use, leaving the agent to ask for a file the system already has.
//
// The handle is validated against the session's own turns so an agent that
// mistypes or invents one is told so, rather than told "pinned" and then shown
// nothing. Validation is a courtesy, not the security boundary: hydration
// consults pins only for refs already in this conversation, so an unknown
// handle is inert whether or not it was rejected here.
func (l *Loop) PinAttachment(ctx context.Context, handle string) error {
	if handle == "" {
		return ErrAttachmentNotInSession
	}
	if l.Memory == nil {
		return fmt.Errorf("pin %q: no memory store wired", handle)
	}
	turns, err := l.Memory.ReadAll(ctx)
	if err != nil {
		return fmt.Errorf("pin %q: read session turns: %w", handle, err)
	}
	found := false
	for _, t := range turns {
		for _, b := range t.Content {
			if b.Attachment != nil && b.Attachment.Ref == handle {
				found = true
			}
		}
	}
	if !found {
		return ErrAttachmentNotInSession
	}

	l.pinnedMu.Lock()
	defer l.pinnedMu.Unlock()
	if l.pinnedAttachments == nil {
		l.pinnedAttachments = map[string]bool{}
	}
	l.pinnedAttachments[handle] = true
	slog.Default().Info("attachment pinned back into view by the agent",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "handle", handle)
	return nil
}

// isPinned reports whether handle was pinned by show_attachment.
func (l *Loop) isPinned(handle string) bool {
	l.pinnedMu.Lock()
	defer l.pinnedMu.Unlock()
	return l.pinnedAttachments[handle]
}
