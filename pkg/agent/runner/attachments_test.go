package runner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// --- helpers -----------------------------------------------------------

// stubReader implements modality.ArtifactReader, returning the same fixed
// bytes for every ref regardless of range.
type stubReader struct{ data []byte }

func (s stubReader) ReadRange(_ context.Context, _ artifactstore.Ref, _, _ int64) ([]byte, int64, error) {
	return s.data, int64(len(s.data)), nil
}

// errReader implements modality.ArtifactReader by always failing — the
// ArtifactReader-error path, distinct from a nil reader. Mirrors the
// errReader in pkg/agent/modality/files/fetch_artifact_test.go.
type errReader struct{}

func (errReader) ReadRange(_ context.Context, _ artifactstore.Ref, _, _ int64) ([]byte, int64, error) {
	return nil, 0, artifactstore.ErrNotFound
}

// perRefFailReader implements modality.ArtifactReader, succeeding with fixed
// bytes for every ref EXCEPT failRef, which always errors. Used to exercise
// hydrateAttachments' mixed render/no-render path WITHIN a single message —
// one eligible attachment fails to fetch while its siblings render
// successfully — which a reader that fails uniformly (errReader) cannot
// reach.
type perRefFailReader struct {
	data    []byte
	failRef string
}

func (r perRefFailReader) ReadRange(_ context.Context, ref artifactstore.Ref, _, _ int64) ([]byte, int64, error) {
	if string(ref) == r.failRef {
		return nil, 0, artifactstore.ErrNotFound
	}
	return r.data, int64(len(r.data)), nil
}

// newLoopWithNativeSet builds a Loop whose provider declares exactly the
// given native MIME set and whose ArtifactReader returns fixed bytes.
func newLoopWithNativeSet(t *testing.T, set llm.MIMESet, data []byte) *runner.Loop {
	t.Helper()
	p := &fake.Provider{}
	p.SetNativeInputMIMEs(set)
	return &runner.Loop{Provider: p, Model: "demo-model", ArtifactReader: stubReader{data: data}}
}

// newLoopWithNativePNG is the common case: a model that takes PNG natively.
func newLoopWithNativePNG(t *testing.T, data []byte) *runner.Loop {
	t.Helper()
	return newLoopWithNativeSet(t, llm.NewMIMESet(map[string]string{
		"image/png": llm.NativeBlockImage,
	}), data)
}

// messagesWithAttachment builds a conversation carrying one attachment
// (mime, textRef) whose eligibility can be tested along the head-turn axis.
// When headTurn is false, a SECOND message is appended: a plain text-only
// user turn — the natural reading of "a later turn happened", and what
// actually moves hydrateAttachments' head-turn boundary (the most recent
// message a human authored — see mostRecentUserTurn's doc in
// attachments.go). It is deliberately NOT another attachment-carrying
// message and NOT a tool-result-shaped one: the former would only prove
// the boundary moves, not that plain conversation moves it too; the latter
// would prove the opposite of what "older turn" here means to test (a
// tool-result relay must NOT move the boundary — see
// TestHydrateAttachments_HeadTurn_SurvivesOwnMultiStepToolCalls for that
// case specifically).
func messagesWithAttachment(mime, textRef string, headTurn bool) []llm.Message {
	target := llm.Message{Role: "user", Content: []llm.ContentBlock{
		{Type: "text", Text: "here is a file"},
		{Type: "attachment", Attachment: &llm.AttachmentRef{
			Ref: "mem://ns/sess/inbound-asset/target/raw/file", MIME: mime, TextRef: textRef, Filename: "file",
		}},
	}}
	if headTurn {
		return []llm.Message{target}
	}
	later := llm.Message{Role: "user", Content: []llm.ContentBlock{
		{Type: "text", Text: "thanks, one more question"},
	}}
	return []llm.Message{target, later}
}

// imageTurn builds one user turn carrying a single image attachment with no
// text fallback — the eviction-candidate shape (nothing else represents it).
//
// The trailing text block is load-bearing, not decoration, and must not be
// "simplified" away: it is what makes
// TestHydrateAttachments_LeavesInputUnmodifiedAndIsIdempotent able to detect
// a destructive pass at all. Every message here is evicted or kept as a
// whole, so the only block clearRemainingRefs removes is the attachment. Were
// the ref LAST, an in-place compaction (`kept := orig[:0]`) would write the
// leading text block back over backing[0] — its own existing value — leave
// backing[1] untouched, and never touch the caller's slice header, so
// require.Equal(before, msgs) would compare EQUAL and the regression would
// ship silently. With a surviving block AFTER the ref, that same compaction
// overwrites backing[1] with a DIFFERENT value and the guard fires. Verified
// empirically by reintroducing the compaction.
//
// Its text deliberately avoids the "img%d.png" filename form, which
// textBlocksMentioning counts to prove the eviction note names each evicted
// file exactly once.
func imageTurn(i int) llm.Message {
	return llm.Message{Role: "user", Content: []llm.ContentBlock{
		{Type: "text", Text: fmt.Sprintf("image %d", i)},
		{Type: "attachment", Attachment: &llm.AttachmentRef{
			Ref:      fmt.Sprintf("mem://ns/sess/inbound-asset/%d/raw/img.png", i),
			MIME:     "image/png",
			Filename: fmt.Sprintf("img%d.png", i),
		}},
		{Type: "text", Text: fmt.Sprintf("what do you make of image %d?", i)},
	}}
}

// messagesWithNImages builds n one-image-per-message turns, oldest first, so
// the newest-N window and oldest-first eviction can be tested directly
// against message order. None carries a text fallback — nothing else
// represents these attachments, which is why they're eviction candidates
// at all.
func messagesWithNImages(t *testing.T, n int) []llm.Message {
	t.Helper()
	msgs := make([]llm.Message, n)
	for i := range n {
		msgs[i] = imageTurn(i)
	}
	return msgs
}

// markerNonces returns the nonce carried by every OPENING attachment marker
// across msgs, in conversation order. Closing markers start "</", so the
// prefix check picks up each region exactly once.
func markerNonces(t *testing.T, msgs []llm.Message) []string {
	t.Helper()
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if strings.HasPrefix(b.Text, "<"+untrusted.AttachmentTag) {
				out = append(out, nonceFrom(t, b.Text))
			}
		}
	}
	return out
}

// copyMessages copies msgs deeply enough to detect any in-place mutation of
// the messages, their content blocks, OR the AttachmentRefs those blocks
// point at: the message structs, each Content slice, and each ref are all
// copied.
//
// Copying the refs is what closes the last hole. Sharing the pointers made
// require.Equal follow both sides to the same object, so a pass that wrote
// THROUGH a ref — normalizing a MIME, rewriting a handle — would compare
// equal and the idempotence guard would not see it. Nothing writes through
// them today; that is the property being guarded, not an assumption the
// guard gets to make.
func copyMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Content = append([]llm.ContentBlock(nil), msgs[i].Content...)
		for j := range out[i].Content {
			if ref := out[i].Content[j].Attachment; ref != nil {
				clone := *ref
				out[i].Content[j].Attachment = &clone
			}
		}
	}
	return out
}

// indexOfType returns the index of the first block of blockType among
// blocks, or -1 if none. Takes a raw block slice (not a Message) so it can
// be applied both to a whole message's Content and to slices carved out
// mid-test (e.g. the brackets around a native block).
func indexOfType(blocks []llm.ContentBlock, blockType string) int {
	for i, b := range blocks {
		if b.Type == blockType {
			return i
		}
	}
	return -1
}

// nonceFrom extracts the nonce="..." value from a marker string built by
// fmt.Sprintf's %q (e.g. `<untrusted-attachment nonce="abcd1234" ...>`).
// Returns "" if no nonce attribute is present.
func nonceFrom(t *testing.T, marker string) string {
	t.Helper()
	const key = `nonce="`
	i := strings.Index(marker, key)
	if i < 0 {
		return ""
	}
	rest := marker[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// hasNativeBlock reports whether any message carries a native (image or
// document) block.
func hasNativeBlock(msgs []llm.Message) bool {
	for _, m := range msgs {
		if indexOfType(m.Content, llm.NativeBlockImage) >= 0 || indexOfType(m.Content, llm.NativeBlockDocument) >= 0 {
			return true
		}
	}
	return false
}

// countNativeBlocks counts native (image or document) blocks across msgs.
func countNativeBlocks(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.NativeBlockImage || b.Type == llm.NativeBlockDocument {
				n++
			}
		}
	}
	return n
}

// countRefs counts surviving "attachment" reference blocks across msgs —
// used to reassert post-condition 1 (no ref survives hydration) alongside
// scenario-specific assertions.
func countRefs(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "attachment" {
				n++
			}
		}
	}
	return n
}

// newestAreKept reports whether exactly the newest runner.MaxPersistentNativeBlocks
// messages (by position in msgs) carry a native image block — proving
// eviction drops the oldest candidates first.
func newestAreKept(msgs []llm.Message) bool {
	n := len(msgs)
	for i, m := range msgs {
		wantNative := i >= n-runner.MaxPersistentNativeBlocks
		gotNative := indexOfType(m.Content, llm.NativeBlockImage) >= 0
		if gotNative != wantNative {
			return false
		}
	}
	return true
}

// textBlocksMentioning returns the text of every block across msgs that
// mentions want. A rendered attachment's own untrusted marker carries its
// filename, so a filename that appears in NO block is one nothing represents
// — and a filename that appears TWICE is a note that accumulated.
func textBlocksMentioning(msgs []llm.Message, want string) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if strings.Contains(b.Text, want) {
				out = append(out, b.Text)
			}
		}
	}
	return out
}

// allowedBlockTypes are every ContentBlock.Type value hydrateAttachments may
// legitimately leave behind: the pre-existing conversation types plus the
// two native block kinds this pass introduces. Anything else — including
// the empty string a registry entry with an empty declared block type would
// otherwise leak (Finding 1: native.Has can be true while BlockType is ""),
// or a surviving "attachment" ref, or a hardcoded native.BlockType value
// that doesn't match what the registry actually declared — is exactly the
// unknown-content-block-type failure post-condition 1 exists to prevent.
var allowedBlockTypes = map[string]bool{
	"text": true, "tool_use": true, "tool_result": true, "container_upload": true,
	llm.NativeBlockImage: true, llm.NativeBlockDocument: true,
}

// assertOnlyKnownBlockTypes asserts every block across msgs has a type a
// provider adapter actually understands. An implementation that omitted the
// native.Has gate entirely, or the empty-BlockType guard in nativeBlockFor,
// would still pass hasNativeBlock/countRefs-only assertions (that block's
// Type is "", which neither function looks for) — this closes that hole.
func assertOnlyKnownBlockTypes(t *testing.T, msgs []llm.Message) {
	t.Helper()
	for _, m := range msgs {
		for _, b := range m.Content {
			assert.True(t, allowedBlockTypes[b.Type], "block type %q must not survive hydration", b.Type)
		}
	}
}

// --- tests ---------------------------------------------------------------

// TestHydrateAttachments_LeavesInputUnmodifiedAndIsIdempotent is the guard
// for the pass's central structural property: it builds ONE REQUEST'S view of
// the conversation and leaves the caller's slice alone.
//
// The slice the loop hands in is its long-lived, append-only conversation —
// every other site does `messages = append(messages, …)` and nothing ever
// rebuilds it. A pass that hydrated in place would therefore be a one-way
// door: a rendered attachment is no longer Type "attachment", so the next
// request's pass could not see it, and neither the newest-N window nor the
// per-request byte budget could count what is already attached. Rendering in
// place would also rewrite an EARLY message late in a session, churning the
// prompt-cache prefix from that message onward.
//
// Both halves matter and neither implies the other: leaving the input alone
// is what makes the pass re-derivable, and producing the same result from the
// same input is what makes "re-derived every request" stable rather than
// drifting.
func TestHydrateAttachments_LeavesInputUnmodifiedAndIsIdempotent(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	msgs := messagesWithNImages(t, runner.MaxPersistentNativeBlocks+2)
	before := copyMessages(msgs)

	first := copyMessages(runner.HydrateAttachmentsForTest(l, context.Background(), msgs))
	second := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	require.Equal(t, before, msgs,
		"hydration must not mutate the loop's long-lived conversation slice — a rendered attachment is no longer a ref, so a destructive pass can never re-derive the window or the byte budget")
	assert.Equal(t, first, second,
		"the same conversation must hydrate to the byte-identical request every time — anything that differs per request re-costs the cached prefix from that block onward")
}

// TestHydrateAttachments_WindowBoundsNativeBlocksAcrossTurns proves the
// newest-N window bounds what is ATTACHED, not merely how many refs one pass
// renders. This drives the pass the way the loop does: one message appended
// per turn, hydrating the whole grown conversation before each send. A pass
// that rendered into the conversation itself would bound only the first
// turn — after that the already-rendered blocks are invisible to it, and an
// image thread grows a native block per turn without limit until the provider
// rejects the request.
func TestHydrateAttachments_WindowBoundsNativeBlocksAcrossTurns(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	var conv []llm.Message

	for turn := range runner.MaxPersistentNativeBlocks + 4 {
		conv = append(conv, imageTurn(turn))
		sent := runner.HydrateAttachmentsForTest(l, context.Background(), conv)

		assert.LessOrEqual(t, countNativeBlocks(sent), runner.MaxPersistentNativeBlocks,
			"turn %d: attached native blocks must stay inside the window across turns, not just within one pass", turn)
		assert.Equal(t, 0, countRefs(sent), "turn %d: no attachment ref may survive hydration", turn)
		assertOnlyKnownBlockTypes(t, sent)
	}

	final := runner.HydrateAttachmentsForTest(l, context.Background(), conv)
	assert.Equal(t, runner.MaxPersistentNativeBlocks, countNativeBlocks(final),
		"the window must still be full at the end — bounding must not mean dropping everything")
	assert.True(t, newestAreKept(final), "the surviving native blocks must be the newest ones")
}

// TestHydrateAttachments_NoRefEverSurvives is the post-condition guard for
// the hydration pass. The one risk of hydrating in a post-pass is an
// attachment ref escaping to a provider adapter, which hard-errors on
// unknown block types. Rather than rely on every branch remembering to
// convert, the pass clears refs unconditionally. This asserts that,
// including for a MIME no model claims.
func TestHydrateAttachments_NoRefEverSurvives(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: []llm.ContentBlock{
		{Type: "text", Text: "here you go"},
		{Type: "attachment", Attachment: &llm.AttachmentRef{
			Ref: "mem://a", MIME: "application/x-nonesuch", Filename: "weird.bin",
		}},
	}}}

	l := &runner.Loop{Provider: &fake.Provider{}, Model: "no-such-model"}
	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	for _, m := range got {
		for _, b := range m.Content {
			assert.NotEqual(t, "attachment", b.Type,
				"no attachment ref may survive hydration — adapters hard-error on it")
			assert.Nil(t, b.Attachment, "the ref field must be cleared too")
		}
	}
}

// TestHydrateAttachments_AttachmentOnlyMessage_FallsBackToPlaceholder is the
// regression guard for the defect fix round 1 found in this pass: a message
// whose ONLY content block is an attachment ref. contentBlocksFromMemory now
// keeps such a turn non-empty by carrying the ref through in reference
// form — but that guard runs BEFORE this pass exists to clear the ref. Without
// a second check here, clearing the last block leaves the message with zero
// content blocks, which is itself a distinct provider error from an unknown
// block type ("a message needs at least one block"), not merely a milder
// version of the ref-leak the first post-condition guards. This is the exact
// shape ("an attachment arrives with no accompanying text") that would have
// shipped broken: the first LLM request on such a turn would hard-error, same
// user-facing failure as the original R2-2 bug this whole pipeline exists to
// prevent.
func TestHydrateAttachments_AttachmentOnlyMessage_FallsBackToPlaceholder(t *testing.T) {
	// TextRef is set deliberately. Without it this ref lands in the
	// `unreadable` bucket, appendUnreadableNote fills the message, and the
	// placeholder below never runs — the test would pass on the note and
	// stop guarding the branch it names. A text fallback suppresses the note
	// (the manifest line and fetch_artifact handle still describe the file),
	// so stripping the ref genuinely empties the message and the placeholder
	// is the only thing that can refill it.
	msgs := []llm.Message{{Role: "user", Content: []llm.ContentBlock{
		{Type: "attachment", Attachment: &llm.AttachmentRef{
			Ref: "mem://orphan", MIME: "application/pdf", Filename: "orphan.pdf",
			TextRef: "mem://orphan-text",
		}},
	}}}

	l := &runner.Loop{Provider: &fake.Provider{}, Model: "no-such-model"}
	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	require.Len(t, got[0].Content, 1,
		"an empty Content slice is a provider error distinct from an unknown block type; hydration must fall back to a placeholder rather than leave the message empty")
	assert.Equal(t, "text", got[0].Content[0].Type)
	// Assert the exact placeholder, not merely non-empty: any note this pass
	// writes is also a non-empty text block, so a weaker assertion cannot
	// tell "the placeholder fired" from "something else filled the message"
	// — which is how this test previously stopped covering its own branch.
	assert.Equal(t, runner.EmptyContentPlaceholderTextForTest, got[0].Content[0].Text,
		"the message must be refilled by the placeholder specifically")

	params, err := anthropic.BuildParams(llm.Request{
		Model:     "claude-opus-4-8",
		MaxTokens: 16,
		Messages:  got,
	})
	require.NoError(t, err, "the placeholder fallback must itself produce a valid LLM request, not an 'unknown content block type' or empty-message error")
	require.Len(t, params.Messages, 1)
	require.Len(t, params.Messages[0].Content, 1)
}

// TestHydrateAttachments_EligibilityMatrix is the full eligibility matrix.
// Native rendering requires the model to declare the MIME AND the
// attachment to be eligible by lifetime:
//   - TextRef set    → head turn only (extracted text represents it after)
//   - TextRef empty  → persists, within the newest-N window (nothing else does)
func TestHydrateAttachments_EligibilityMatrix(t *testing.T) {
	const png, pptx = "image/png", "application/vnd.openxmlformats-officedocument.presentationml.presentation"

	cases := []struct {
		name       string
		mime       string
		textRef    string
		headTurn   bool
		wantNative bool
	}{
		{name: "declared MIME, no text fallback, head turn: native", mime: png, headTurn: true, wantNative: true},
		{name: "declared MIME, no text fallback, older turn: native (persists)", mime: png, headTurn: false, wantNative: true},
		{name: "declared MIME, has text fallback, head turn: native", mime: png, textRef: "mem://t", headTurn: true, wantNative: true},
		{name: "declared MIME, has text fallback, older turn: dropped (text carries it)", mime: png, textRef: "mem://t", headTurn: false, wantNative: false},
		{name: "undeclared MIME, head turn: dropped", mime: pptx, textRef: "mem://t", headTurn: true, wantNative: false},
		{name: "undeclared MIME, no text fallback, head turn: dropped", mime: pptx, headTurn: true, wantNative: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
			msgs := messagesWithAttachment(tc.mime, tc.textRef, tc.headTurn)

			got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

			assert.Equal(t, tc.wantNative, hasNativeBlock(got),
				"native-block presence must follow declared-MIME AND lifetime eligibility")
			assert.Equal(t, 0, countRefs(got), "no attachment ref may survive hydration regardless of eligibility outcome")
			assertOnlyKnownBlockTypes(t, got)
		})
	}
}

// TestNativeSupportIsRegistryDriven is the requirement in one test: whether a
// format is sent natively is decided ONLY by the model registry. The same
// attachment, the same conversation, and the same code produce different
// behavior for no reason other than what the declared MIME set says.
//
// That is the property the whole design rests on — adding a format when a
// provider starts accepting it must be a one-row edit to
// pkg/agent/llm/models/models.go and nothing else. If this test ever fails,
// some consumer between the registry and the provider call has grown its own
// opinion about MIME types, and the one-row edit is no longer sufficient: a
// MIME moved into a model's row would still not be sent natively.
//
// The MIME chosen is deliberately one NO model row declares today (a PPTX),
// so the "declared" case cannot pass by accident through the real Anthropic
// table.
//
// Exactness note: the one-row edit is bounded by the SDK on BOTH native block
// types, not only on documents. A document format needs a matching SDK source
// variant in the adapter's document case (TestOnlyPDFMapsToNativeBlockDocument,
// pkg/agent/llm/models). An image format needs a value in the SDK's
// Base64ImageSourceMediaType enum, which the adapter casts the MIME into
// unchecked — four values today, and a MIME outside them is a 400 rather than
// a compile error (TestEveryNativeImageMIMEIsAnSDKMediaType,
// pkg/agent/llm/anthropic). This test weakens neither guard: it drives
// hydration, where the registry genuinely is the only opinion, not the
// Anthropic adapter, where the SDK's enum is the real bound.
func TestNativeSupportIsRegistryDriven(t *testing.T) {
	const pptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"

	t.Run("undeclared: rendered as a reference", func(t *testing.T) {
		l := newLoopWithNativeSet(t, llm.NewMIMESet(nil), []byte("PPTXBYTES"))

		got := runner.HydrateAttachmentsForTest(l, context.Background(), messagesWithAttachment(pptx, "", true))

		assert.False(t, hasNativeBlock(got), "an undeclared MIME must not be sent natively")
		assert.Equal(t, 0, countRefs(got), "no attachment ref may survive hydration on the undeclared path either")
		assertOnlyKnownBlockTypes(t, got)
	})

	t.Run("declared: rendered natively, nothing else changed", func(t *testing.T) {
		l := newLoopWithNativeSet(t, llm.NewMIMESet(map[string]string{
			pptx: llm.NativeBlockDocument,
		}), []byte("PPTXBYTES"))

		got := runner.HydrateAttachmentsForTest(l, context.Background(), messagesWithAttachment(pptx, "", true))

		require.True(t, hasNativeBlock(got), "declaring the MIME must be sufficient, on its own, to send it natively")
		// The DECLARED block type, not merely "some native block": a consumer
		// that decided the type itself (rather than reading it back from the
		// registry) would still satisfy hasNativeBlock while making the
		// registry's mapping half a dead letter.
		idx := indexOfType(got[0].Content, llm.NativeBlockDocument)
		require.GreaterOrEqual(t, idx, 0, "the block type must be the one the registry declared for this MIME")
		assert.Equal(t, pptx, got[0].Content[idx].MIME)
		assert.Equal(t, []byte("PPTXBYTES"), got[0].Content[idx].Data)
		assert.Equal(t, 0, countRefs(got))
		assertOnlyKnownBlockTypes(t, got)
	})
}

// TestHydrateAttachments_HeadTurn_SurvivesOwnMultiStepToolCalls proves the
// head-turn boundary is "the most recent message a human authored", not
// "the most recent message": a Role:"user" message the runner itself
// synthesizes to carry tool_result blocks back to the provider (an
// Anthropic API mechanic — tool results ride in a "user"-role message
// regardless of who said anything) must NOT expire an attachment that
// arrived earlier in the same multi-step agent turn. The agent may still
// need the document while it is mid-tool-loop working on the user's
// original request.
func TestHydrateAttachments_HeadTurn_SurvivesOwnMultiStepToolCalls(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	msgs := []llm.Message{
		{Role: "user", Content: []llm.ContentBlock{
			{Type: "text", Text: "here is a file, use a tool on it"},
			{Type: "attachment", Attachment: &llm.AttachmentRef{
				Ref: "mem://ns/sess/inbound-asset/target/raw/file", MIME: "image/png", TextRef: "mem://t", Filename: "file",
			}},
		}},
		{Role: "assistant", Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "t1", Name: "some_tool"}},
		}},
		{Role: "user", Content: []llm.ContentBlock{
			{Type: "tool_result", ToolResult: &llm.ToolResultBlock{ToolUseID: "t1", Content: "ok"}},
		}},
	}

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.True(t, hasNativeBlock(got),
		"a tool-result relay message (Role:\"user\" but not human-authored) must not expire the attachment's own multi-step turn")
	assertOnlyKnownBlockTypes(t, got)
}

// TestHydrateAttachments_BlockTypeAndDataComeFromRegistry proves the
// resulting native block's Type is read from the registry's declared
// mapping for the MIME, not hardcoded to image — an implementation that
// hardcoded llm.NativeBlockImage (the exact strings.HasPrefix-shaped
// shortcut the global constraint forbids) would pass every other test in
// this file, since hasNativeBlock accepts image OR document. Also proves
// the block's bytes actually came from the ArtifactReader.
func TestHydrateAttachments_BlockTypeAndDataComeFromRegistry(t *testing.T) {
	const pdf = "application/pdf"
	data := []byte("PDFBYTES")
	l := newLoopWithNativeSet(t, llm.NewMIMESet(map[string]string{
		pdf: llm.NativeBlockDocument,
	}), data)
	msgs := messagesWithAttachment(pdf, "", true)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	idx := indexOfType(got[0].Content, llm.NativeBlockDocument)
	require.GreaterOrEqual(t, idx, 0, "a document-mapped MIME must produce a document block, not an image block")
	assert.Equal(t, llm.NativeBlockDocument, got[0].Content[idx].Type)
	assert.Equal(t, pdf, got[0].Content[idx].MIME)
	assert.Equal(t, data, got[0].Content[idx].Data, "block bytes must come from the ArtifactReader")
}

// TestHydrateAttachments_EmptyRegistryBlockType_DegradesGracefully is the
// regression guard for Finding 1: NewMIMESet performs no validation, so a
// model row can map a MIME to an empty block type (a one-character typo in
// exactly the "adding a format is a one-row edit" workflow this feature
// advertises, e.g. {"image/heic": ""}). native.Has reports true for that
// key regardless — only BlockType exposes the emptiness. Unguarded, this
// reaches the ArtifactReader, gets real bytes back, and produces
// llm.ContentBlock{Type: ""}: not "attachment", so clearRemainingRefs would
// not strip it, and it would reach the provider adapter as "unknown content
// block type """ — the exact failure post-condition 1 exists to prevent,
// arriving through a different door.
func TestHydrateAttachments_EmptyRegistryBlockType_DegradesGracefully(t *testing.T) {
	l := newLoopWithNativeSet(t, llm.NewMIMESet(map[string]string{
		"image/heic": "",
	}), []byte("HEICBYTES"))
	msgs := messagesWithAttachment("image/heic", "", true)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.Equal(t, 0, countNativeBlocks(got), "an empty registry block type must not produce a native block")
	assert.Equal(t, 0, countRefs(got), "no attachment ref may survive hydration either")
	assertOnlyKnownBlockTypes(t, got)
}

// TestHydrateAttachments_ReadFailure_DegradesGracefully covers the
// ArtifactReader-error path — distinct from the nil-reader path below — to
// prove a failing read also degrades to reference form rather than
// panicking or letting the error propagate and fail the turn.
func TestHydrateAttachments_ReadFailure_DegradesGracefully(t *testing.T) {
	l := newLoopWithNativePNG(t, nil)
	l.ArtifactReader = errReader{}
	msgs := messagesWithAttachment("image/png", "", true)

	var got []llm.Message
	require.NotPanics(t, func() {
		got = runner.HydrateAttachmentsForTest(l, context.Background(), msgs)
	})

	assert.False(t, hasNativeBlock(got), "a read failure must not produce a native block")
	assert.Equal(t, 0, countRefs(got))
	assertOnlyKnownBlockTypes(t, got)
}

// TestHydrateAttachments_EmptyRead_DegradesGracefully is the regression guard
// for the session-killer: a read that SUCCEEDS and returns zero bytes.
//
// A 0-byte upload is reachable — the operator's inbound-asset route enforces a
// maximum but no minimum and does not sniff content, and the stored MIME is a
// hint, not a verified fact. Unguarded, the empty read produced a native block
// whose Data was empty, which the adapter faithfully encodes as an empty
// base64 source and the API rejects with a 400. That is not a one-turn
// failure: no TextRef means the attachment lives in the PERSISTENT window, so
// every retry rebuilds the identical block until the retry budget is gone and
// the session fails terminally — and the durable turn replays it after a
// restart. Degrading to reference form costs one file instead.
//
// The wire-level assertion is the point. Asserting only "no native block"
// would pass for an implementation that emitted the block with a different
// type; asserting the built provider params carry no empty image source pins
// the actual failure the provider sees.
func TestHydrateAttachments_EmptyRead_DegradesGracefully(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte{}) // the read succeeds, with nothing in it
	msgs := messagesWithAttachment("image/png", "", true)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.Equal(t, 0, countNativeBlocks(got), "a zero-byte read must not produce a native block")
	assert.Equal(t, 0, countRefs(got), "no attachment ref may survive hydration on the empty-read degrade path either")
	assertOnlyKnownBlockTypes(t, got)

	params, err := anthropic.BuildParams(llm.Request{Model: "claude-opus-4-8", MaxTokens: 16, Messages: got})
	require.NoError(t, err)
	for _, m := range params.Messages {
		for _, c := range m.Content {
			if c.OfImage == nil || c.OfImage.Source.OfBase64 == nil {
				continue
			}
			assert.NotEmpty(t, c.OfImage.Source.OfBase64.Data,
				"an empty base64 image source is a provider 400 that every retry reproduces — the block must never be built at all")
		}
	}
}

// TestHydrateAttachments_EvictsOldestBeyondWindow proves only the newest N
// attachments with no text fallback stay attached; older ones revert to
// reference form so a long image thread cannot grow without bound. Eviction
// is oldest-first — the newest image is the one most likely to matter.
func TestHydrateAttachments_EvictsOldestBeyondWindow(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	msgs := messagesWithNImages(t, runner.MaxPersistentNativeBlocks+2)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.Equal(t, runner.MaxPersistentNativeBlocks, countNativeBlocks(got),
		"window must bound how many native blocks are attached")
	assert.True(t, newestAreKept(got), "eviction must be oldest-first — the newest image is the one most likely to matter")
	assert.Equal(t, 0, countRefs(got), "evicted attachments must not leave a dangling ref behind")
	assertOnlyKnownBlockTypes(t, got)
}

// TestHydrateAttachments_NilArtifactReader_DegradesGracefully proves the nil
// case documented on Loop.ArtifactReader: an attachment otherwise eligible
// for a native block (declared MIME, no text fallback, head turn) must
// degrade to reference form — here, simply dropped, since a sibling
// manifest text line already describes it — rather than panic on a nil
// interface call.
func TestHydrateAttachments_NilArtifactReader_DegradesGracefully(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	l.ArtifactReader = nil
	msgs := messagesWithAttachment("image/png", "", true)

	var got []llm.Message
	require.NotPanics(t, func() {
		got = runner.HydrateAttachmentsForTest(l, context.Background(), msgs)
	})

	assert.False(t, hasNativeBlock(got), "a nil ArtifactReader must not produce a native block")
	assert.Equal(t, 0, countRefs(got), "no attachment ref may survive hydration even on the nil-reader degrade path")
	assertOnlyKnownBlockTypes(t, got)
}

// A native block carries file content into the most-trusted position in the
// conversation: a user-role message. It must be bracketed with the same
// nonce discipline as tool output, or a file can impersonate the user.
func TestHydrateAttachments_BracketsNativeBlocksAsUntrusted(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	msgs := messagesWithAttachment("image/png", "", true)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	blocks := got[0].Content
	i := indexOfType(blocks, llm.NativeBlockImage)
	require.Greater(t, i, 0, "a native block must be preceded by an opening marker")
	require.Less(t, i, len(blocks)-1, "a native block must be followed by a closing marker")

	// Assert the OPEN/CLOSE forms explicitly, not just tag presence:
	// assert.Contains(closing, untrusted.AttachmentTag) alone would pass
	// just as happily for a second "<untrusted-attachment ...>" (i.e. a
	// missing "/") as for the real closing tag — which means nothing would
	// terminate the untrusted region. HasPrefix on "<"+tag pins the opening
	// form; Contains on "</"+tag pins the closing form.
	open, closing := blocks[i-1].Text, blocks[i+1].Text
	assert.True(t, strings.HasPrefix(open, "<"+untrusted.AttachmentTag), "opening marker must start with <%s, got %q", untrusted.AttachmentTag, open)
	assert.True(t, strings.Contains(closing, "</"+untrusted.AttachmentTag), "closing marker must contain </%s, got %q", untrusted.AttachmentTag, closing)

	nonce := nonceFrom(t, open)
	assert.NotEmpty(t, nonce, "opening marker must carry a nonce")
	assert.Equal(t, nonce, nonceFrom(t, closing), "closing nonce must match the opening one")
}

// TestHydrateAttachments_NonceIsStablePerAttachmentAndDiffersBetweenThem pins
// both halves of the nonce contract.
//
// DIFFERENT attachments must never share a nonce: a matched pair is what
// bounds one untrusted region, and two regions sharing a nonce would let the
// close of one end the other.
//
// The SAME attachment must keep its nonce across requests. Hydration
// re-derives every native block from its ref on every request, so a nonce
// minted per call would rewrite the marker text either side of every attached
// block every turn — and a cache breakpoint only helps up to the first changed
// block, so one image early in a long conversation would re-cost everything
// after it, on every single turn.
//
// Stability costs nothing the threat model cares about. The property that
// matters is that a nonce is unforgeable BY THE FILE IT WRAPS, and that holds
// however long the nonce lives: the bytes are frozen at upload, before the
// nonce is minted, so a file cannot contain its own closing marker. Re-minting
// per request would only defend against a model that leaked a live nonce into
// its own output AND an attacker who could then land a later file in the same
// conversation — a thin margin, and not worth the whole cached prefix.
func TestHydrateAttachments_NonceIsStablePerAttachmentAndDiffersBetweenThem(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	msgs := []llm.Message{{Role: "user", Content: []llm.ContentBlock{
		{Type: "text", Text: "two files"},
		{Type: "attachment", Attachment: &llm.AttachmentRef{Ref: "mem://a", MIME: "image/png", Filename: "a.png"}},
		{Type: "attachment", Attachment: &llm.AttachmentRef{Ref: "mem://b", MIME: "image/png", Filename: "b.png"}},
	}}}

	first := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)
	nonces := markerNonces(t, first)
	require.Len(t, nonces, 2, "both attachments must render with an opening marker")
	assert.NotEmpty(t, nonces[0])
	assert.NotEqual(t, nonces[0], nonces[1],
		"two attachments in one request must never share a nonce — one region's close would end the other")

	second := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.Equal(t, nonces, markerNonces(t, second),
		"an attachment must keep its own nonce across requests, or every request rewrites the prefix it sits in")
	assert.Equal(t, first, second,
		"an unchanged conversation must hydrate byte-identically — that identity IS the cached prefix")
}

// TestHydrateAttachments_MultipleInOneMessage_EachBracketedCorrectly is the
// regression guard for the index-invalidation risk this task exists to
// avoid: rendering one site turns ONE block into THREE, so pass 1's
// site{msg, block} indices — recorded against the ORIGINAL block
// positions — would point at the wrong block for every attachment AFTER
// the first one in the same message if pass 2 mutated msgs[i].Content in
// place while still consulting those indices. Three attachments in one
// message — two that render natively, one (the middle one, "c") whose
// ArtifactReader.ReadRange fails — prove: each rendered attachment ends up
// bracketed against ITS OWN native block and ITS OWN filename, not shifted
// onto its neighbour by an earlier insertion OR by a sibling that failed to
// render (the mixed render/no-render case: nativeBlockFor's ok=false branch
// must not corrupt the layout pass 2 builds for the REST of the message);
// each rendered attachment gets its OWN nonce, not one shared across the
// whole hydrateAttachments call; and the failed attachment leaves no ref
// behind, appearing only in the head-turn note that tells the agent it cannot
// see that file — never inside another attachment's marker, which is what an
// index bug would produce.
func TestHydrateAttachments_MultipleInOneMessage_EachBracketedCorrectly(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	l.ArtifactReader = perRefFailReader{data: []byte("PNGBYTES"), failRef: "mem://c"}
	msgs := []llm.Message{{Role: "user", Content: []llm.ContentBlock{
		{Type: "text", Text: "two files"},
		{Type: "attachment", Attachment: &llm.AttachmentRef{Ref: "mem://a", MIME: "image/png", Filename: "a.png"}},
		{Type: "text", Text: "and a third that will fail to fetch"},
		{Type: "attachment", Attachment: &llm.AttachmentRef{Ref: "mem://c", MIME: "image/png", Filename: "c.png"}},
		{Type: "text", Text: "and another"},
		{Type: "attachment", Attachment: &llm.AttachmentRef{Ref: "mem://b", MIME: "image/png", Filename: "b.png"}},
	}}}

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	blocks := got[0].Content
	assert.Equal(t, 2, countNativeBlocks(got), "only the two fetchable attachments must render natively")
	assert.Equal(t, 0, countRefs(got), "no attachment ref may survive hydration, including the one that failed to fetch")
	assertOnlyKnownBlockTypes(t, got)
	// The file that could not be fetched has no text fallback, so nothing in
	// the request represents it: it is named exactly once, by the note, and
	// never inside a sibling's untrusted marker (which is what an index bug
	// shifting filenames onto the wrong block would look like).
	cMentions := textBlocksMentioning(got, "c.png")
	require.Len(t, cMentions, 1, "an unfetchable attachment with no text fallback must be named exactly once")
	assert.Contains(t, cMentions[0], "not readable", "its one mention must be the note that tells the agent it cannot see the file")
	assert.NotContains(t, cMentions[0], untrusted.AttachmentTag, "the failed attachment must never appear inside another attachment's marker")

	// Every native block's immediate neighbours must be its OWN matched
	// open/close pair, never a neighbour's leftover block or a mismatched
	// nonce from an insertion earlier in the same message. Assert the
	// OPEN/CLOSE forms explicitly (HasPrefix "<tag" / Contains "</tag"): a
	// missing "/" on the closing marker (open, native, open) would still
	// satisfy a bare Contains(closing, tag) check on both sides.
	var nonces []string
	seen := 0
	for i, b := range blocks {
		if b.Type != llm.NativeBlockImage {
			continue
		}
		seen++
		require.Greater(t, i, 0)
		require.Less(t, i, len(blocks)-1)
		open, closing := blocks[i-1].Text, blocks[i+1].Text
		assert.True(t, strings.HasPrefix(open, "<"+untrusted.AttachmentTag), "opening marker must start with <%s, got %q", untrusted.AttachmentTag, open)
		assert.True(t, strings.Contains(closing, "</"+untrusted.AttachmentTag), "closing marker must contain </%s, got %q", untrusted.AttachmentTag, closing)
		n := nonceFrom(t, open)
		assert.NotEmpty(t, n)
		assert.Equal(t, n, nonceFrom(t, closing), "closing nonce must match its own opening nonce")
		nonces = append(nonces, n)
	}
	require.Equal(t, 2, seen, "both fetchable native blocks must be found with a marker on each side")
	assert.NotEqual(t, nonces[0], nonces[1], "each attachment must get its OWN nonce, not one shared across the whole call")

	// Layout after rebuild + clearRemainingRefs + the head-turn note: text,
	// open-a, image-a, close-a, text, [attach-c stripped], text, open-b,
	// image-b, close-b, note — 10 surviving blocks. Asserting the filenames
	// land on the SECOND rendered attachment's own marker (not the first's,
	// and not shifted by the failed one sitting between them) is what would
	// catch an index bug: if pass 2 mutated in place, a stale index would
	// either panic (see the "Extra verification" note in the task report) or
	// silently pick up the wrong block's filename here.
	require.Len(t, blocks, 10)
	assert.Contains(t, blocks[1].Text, "a.png", "first marker must carry the first attachment's filename")
	assert.Contains(t, blocks[6].Text, "b.png", "second marker must carry the second attachment's filename, not the first's or the failed one's")
}

// TestHydrateAttachments_DegradesRatherThanExceedingRequestCap is the
// fail-closed guard on the provider's per-request ceiling. Native blocks are
// re-derived every request and persistent ones accumulate across a session,
// so without a budget a long image thread eventually assembles a request the
// provider rejects outright — a failed turn, which is exactly the silent
// failure class this feature exists to remove. Degrading one attachment to
// reference form costs the agent one file; exceeding the ceiling costs it the
// whole turn.
//
// Which one degrades matters as much as how many: the budget is spent
// newest-first, for the same reason eviction is oldest-first — the file the
// user just sent is the one the turn is probably about.
func TestHydrateAttachments_DegradesRatherThanExceedingRequestCap(t *testing.T) {
	huge := make([]byte, runner.MaxNativeBytesPerRequest) // one alone spends the whole budget
	l := newLoopWithNativePNG(t, huge)
	msgs := messagesWithNImages(t, 2)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.Equal(t, 1, countNativeBlocks(got),
		"the second attachment must degrade rather than push the request past the provider ceiling")
	assert.Equal(t, 0, countRefs(got), "degraded attachments still leave no ref behind")
	assert.Less(t, indexOfType(got[0].Content, llm.NativeBlockImage), 0,
		"the OLDER attachment is the one to degrade when the budget is spent")
	assert.GreaterOrEqual(t, indexOfType(got[1].Content, llm.NativeBlockImage), 0,
		"the newest attachment must get the budget first")
	assertOnlyKnownBlockTypes(t, got)
}

// TestHydrateAttachments_NotesAttachmentsNoLongerVisible covers the agent's
// only signal that a file it could once see is gone. An attachment WITH a
// text fallback needs no note — its manifest line and handle still describe
// it. One WITHOUT a fallback is genuinely out of view, and nothing else in
// the request says so, so the agent would answer about a file it can no
// longer see as though it still could.
//
// The note is re-derived on every request rather than appended once, so the
// test also pins that it appears exactly ONCE per file per request: an
// implementation that appended to the conversation itself would stack a fresh
// copy every turn.
func TestHydrateAttachments_NotesAttachmentsNoLongerVisible(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	msgs := messagesWithNImages(t, runner.MaxPersistentNativeBlocks+2)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	evicted := textBlocksMentioning(got, "img0.png")
	require.Len(t, evicted, 1, "an evicted attachment with no text fallback must be named exactly once")
	assert.Contains(t, evicted[0], "img1.png", "both evicted files belong in the same note")
	assert.NotContains(t, evicted[0], "img5.png", "an attachment still attached must not be reported as gone")

	last := got[len(got)-1].Content
	assert.Contains(t, last[len(last)-1].Text, "img0.png",
		"the note belongs on the head turn, where it cannot churn the cached prefix of older messages")
	assertOnlyKnownBlockTypes(t, got)
}

// TestHydrateAttachments_EvictionNoteDoesNotAccumulateAcrossTurns drives the
// pass the way the loop does — one appended turn per request — and pins that
// each request carries exactly one mention of an evicted file. The note is
// derived from the refs on every pass, so a pass that wrote it back into the
// conversation would leave the agent reading the same eviction announced
// five different times, each on a different turn.
func TestHydrateAttachments_EvictionNoteDoesNotAccumulateAcrossTurns(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	var conv []llm.Message

	for turn := range runner.MaxPersistentNativeBlocks + 4 {
		conv = append(conv, imageTurn(turn))
		sent := runner.HydrateAttachmentsForTest(l, context.Background(), conv)

		// img0 is evicted from the moment the window overflows and stays
		// evicted; every later turn must still mention it exactly once.
		mentions := textBlocksMentioning(sent, "img0.png")
		if turn <= runner.MaxPersistentNativeBlocks-1 {
			assert.Len(t, mentions, 1, "turn %d: still attached — its own untrusted marker names it once", turn)
			continue
		}
		assert.Len(t, mentions, 1, "turn %d: evicted — named once by the note, never accumulating a copy per turn", turn)
		assert.Contains(t, mentions[0], "No longer visible", "turn %d: the mention must be the eviction note", turn)
	}
}

// TestHydrateAttachments_NotesAttachmentsItCannotRead covers the agent's only
// signal about a file whose contents never reached it at all.
//
// Ingestion writes the deliberately neutral "<name> (<type>) was attached."
// for any file it stored without extracting text, because whether the file is
// readable depends on the model resolved at send time. When the answer turns
// out to be "it isn't" — no registry row for the type, or the stored bytes
// could not be read — that neutral line is the agent's ENTIRE signal: a
// filename, no content, no handle, and no statement that it cannot see the
// file. Left there, the agent answers about a file nobody showed it. Only this
// pass knows the resolved model, so only this pass can say so.
//
// A file WITH extracted text is excluded on both paths: its manifest line and
// fetch_artifact handle still represent it, and announcing it unreadable would
// push the agent to ask the user to re-send something it can already read.
func TestHydrateAttachments_NotesAttachmentsItCannotRead(t *testing.T) {
	// heic is deliberately a type the fixture registry does NOT declare; png is
	// declared, so the read-failure cases reach nativeBlockFor and fail there.
	const png, heic = "image/png", "image/heic"

	cases := []struct {
		name      string
		mime      string
		textRef   string
		readFails bool
		wantNote  bool
	}{
		{name: "undeclared type, no text fallback: named, since nothing represents it", mime: heic, wantNote: true},
		{name: "undeclared type, text fallback: not named, the manifest line represents it", mime: heic, textRef: "mem://t"},
		{name: "read fails, no text fallback: named, same class as an undeclared type", mime: png, readFails: true, wantNote: true},
		{name: "read fails, text fallback: not named, the manifest line represents it", mime: png, textRef: "mem://t", readFails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
			if tc.readFails {
				l.ArtifactReader = errReader{}
			}
			msgs := messagesWithAttachment(tc.mime, tc.textRef, true)

			got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

			require.False(t, hasNativeBlock(got), "no case here may render natively — that would test something else entirely")
			mentions := textBlocksMentioning(got, "not readable")
			if !tc.wantNote {
				assert.Empty(t, mentions, "a file its extracted text still carries must not be announced as unreadable")
				return
			}
			require.Len(t, mentions, 1, "the note must be written exactly once per request, never accumulated")
			assert.Contains(t, mentions[0], "file", "the note names the file it is about")
			last := got[len(got)-1].Content
			assert.Equal(t, mentions[0], last[len(last)-1].Text,
				"the note belongs on the head turn, where it cannot churn the cached prefix of older messages")
			assertOnlyKnownBlockTypes(t, got)
		})
	}
}

// TestHydrateAttachments_NoNoteWhenTextFallbackCarriesTheFile proves the note
// is scoped to attachments nothing else represents. A file with extracted
// text expires from native form on the turn after it arrives BY DESIGN — its
// manifest line and text handle still describe it — so announcing it as "no
// longer visible" would be false and would push the agent to ask the user to
// re-send a file it can still read.
func TestHydrateAttachments_NoNoteWhenTextFallbackCarriesTheFile(t *testing.T) {
	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	msgs := messagesWithAttachment("image/png", "mem://t", false)

	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.Empty(t, textBlocksMentioning(got, "No longer visible"),
		"a file its extracted text still represents has not dropped out of view")
	assertOnlyKnownBlockTypes(t, got)
}

// TestHydrateAttachments_SuppressedNativeDegradesAndTellsTheAgent covers the
// recovery half of the malformed-bytes problem. A guard can refuse to build a
// block from bytes it can prove are unusable (a zero-length read), but only
// the provider knows what it will actually accept: a file whose BYTES are
// malformed while its declared MIME looks fine still produces a block the
// provider rejects. Because an attachment with no text fallback lives in the
// persistent window, that block is rebuilt identically on every request — so
// without a suppression switch the retry can only reproduce the failure until
// the budget drains and the session dies.
//
// Suppression makes the pass behave as though the model declared no native
// MIMEs at all, which routes every affected attachment down the existing
// unreadable path: no native block, and a head-turn note so the agent knows
// it cannot see the file rather than answering as if it could.
func TestHydrateAttachments_SuppressedNativeDegradesAndTellsTheAgent(t *testing.T) {
	msgs := messagesWithAttachment("image/png", "", true)

	before := runner.HydrateAttachmentsForTest(newLoopWithNativePNG(t, []byte("PNGBYTES")), context.Background(), msgs)
	require.NotEmpty(t, indexOfTypeAll(before, llm.NativeBlockImage),
		"precondition: without suppression this attachment renders natively")

	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	runner.SuppressNativeBlocksForTest(l)
	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.Empty(t, indexOfTypeAll(got, llm.NativeBlockImage),
		"a suppressed session must not rebuild the block the provider just rejected")
	assert.Empty(t, indexOfTypeAll(got, llm.NativeBlockDocument),
		"suppression covers documents too, not only images")
	assert.NotEmpty(t, textBlocksMentioning(got, "not readable"),
		"the agent must be told it cannot see the file; silence invites it to answer as though it could")
	assertOnlyKnownBlockTypes(t, got)
	assert.Zero(t, countRefs(got), "post-condition 1 still holds under suppression")
}

// indexOfTypeAll returns every (message, block) position carrying typ.
func indexOfTypeAll(msgs []llm.Message, typ string) [][2]int {
	var out [][2]int
	for i := range msgs {
		for j, b := range msgs[i].Content {
			if b.Type == typ {
				out = append(out, [2]int{i, j})
			}
		}
	}
	return out
}

// TestHydrateAttachments_PinnedSurvivesTheWindow proves show_attachment's
// whole point: an attachment the agent asked for by name comes back into
// view even though the newest-N window had evicted it.
//
// Without this the eviction is one-way for exactly the files that need it
// most. An attachment with no extracted text has no other representation —
// fetch_artifact hands back raw bytes a model cannot use — so once it ages
// out, the agent's only recourse is asking the user to re-send a file the
// system is already storing.
func TestHydrateAttachments_PinnedSurvivesTheWindow(t *testing.T) {
	n := runner.MaxPersistentNativeBlocks + 2
	msgs := messagesWithNImages(t, n)
	oldest := msgs[0].Content[1].Attachment.Ref

	l := newLoopWithNativePNG(t, []byte("PNGBYTES"))
	before := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)
	require.Zero(t, len(indexOfTypeAll(before[:1], llm.NativeBlockImage)),
		"precondition: the oldest image is outside the window")

	runner.PinAttachmentForTest(l, oldest)
	got := runner.HydrateAttachmentsForTest(l, context.Background(), msgs)

	assert.NotZero(t, len(indexOfTypeAll(got[:1], llm.NativeBlockImage)),
		"a pinned attachment must be rendered even though the window had evicted it")
	assert.Equal(t, runner.MaxPersistentNativeBlocks+1, len(indexOfTypeAll(got, llm.NativeBlockImage)),
		"a pin ADDS to what is visible; it must not consume a window slot and silently evict a recent file")
	assertOnlyKnownBlockTypes(t, got)
	assert.Zero(t, countRefs(got), "post-condition 1 still holds with a pin in play")
}

// TestHydrateAttachments_OutOfViewNoteCarriesTheHandle guards the note
// against naming a file while withholding the one thing the agent needs to
// act on it. show_attachment takes a handle, so a note that gave only the
// filename would be an instruction the agent cannot follow.
func TestHydrateAttachments_OutOfViewNoteCarriesTheHandle(t *testing.T) {
	msgs := messagesWithNImages(t, runner.MaxPersistentNativeBlocks+1)
	evicted := msgs[0].Content[1].Attachment.Ref

	got := runner.HydrateAttachmentsForTest(newLoopWithNativePNG(t, []byte("PNGBYTES")), context.Background(), msgs)

	notes := textBlocksMentioning(got, "No longer visible")
	require.Len(t, notes, 1, "exactly one out-of-view note")
	assert.Contains(t, notes[0], evicted, "the note must carry the handle show_attachment needs")
	assert.Contains(t, notes[0], "show_attachment", "the note must name the tool that acts on that handle")
}
