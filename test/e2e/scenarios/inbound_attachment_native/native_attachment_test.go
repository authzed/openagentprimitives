//go:build e2e

// Package inbound_attachment_native_test is the end-to-end proof of the
// native-attachment path: a user sends an image over Slack, no extractor
// claims its MIME, and the RAW BYTES reach the provider as a native image
// block — bracketed as untrusted — rather than as a reference the agent
// cannot look at.
//
// It is the counterpart to the fetch_artifact scenario next door. That one
// covers a document WITH extracted text, where the agent reads a handle; this
// one covers a file with NO text fallback, where the only way the agent can
// perceive it at all is the bytes themselves. The two paths diverge inside
// the runner's hydration pass, which is why one scenario cannot stand in for
// the other.
//
// What makes this an e2e test rather than a unit test is the number of
// independent components that have to agree on the same file: the real slack
// listener fetches it, channelsd's gate stores it and writes an attachment
// block naming its ref, the runner's hydration pass resolves that ref through
// the artifact store, and the provider adapter's contract decides what a
// native block may look like. Every one of those has unit tests; none of them
// can catch a ref written in one component's shape and read in another's.
//
// The load-bearing configuration is one line — SetNativeInputMIMEs. It is the
// e2e echo of the property TestNativeSupportIsRegistryDriven pins in
// pkg/agent/runner: native support is decided ONLY by what the model registry
// declares. Nothing in this fixture's AgentClass, Channel, or capability
// grant mentions an image type.
//
// No real names — "user@example.com" is fictional per AGENTS.md, matching
// test/e2e's Options.DefaultUser.
package inbound_attachment_native_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	agentDir = "../../testdata/agent-attachment-native"
	agentCls = "native-attach-agent"

	// humanUserID/humanEmail are the seeded sender identity. TeamID MUST
	// equal fakeslack's AuthTestContext TeamID ("T-test") — the real
	// listener's emailTrusted gate only honors the profile email for a full
	// member of the bot's own installed workspace.
	humanUserID = "U-native-human"
	humanEmail  = "user@example.com"

	pngMIME     = "image/png"
	pngFilename = "screenshot.png"
)

// pngBytes stands in for an uploaded screenshot. Nothing decodes it — the
// point is byte fidelity across five components — but it carries a real PNG
// signature so a future component that DOES sniff the magic number sees what
// the declared MIME promises.
var pngBytes = []byte("\x89PNG\r\n\x1a\ne2e-native-attachment-pixels")

// waitForSingleSession polls until exactly one AgentSession exists in the
// harness's namespace and returns it. The DM channelKey ("dm:<user>")
// deterministically correlates to one session per test, so "exactly one" is
// the expected steady state, not an arbitrary pick.
func waitForSingleSession(t *testing.T, h *e2e.Harness, timeout time.Duration) spiceboxv1alpha1.AgentSession {
	t.Helper()
	var found spiceboxv1alpha1.AgentSession
	e2e.Eventually(t, timeout, func() bool {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(context.Background(), &list); err != nil || len(list.Items) != 1 {
			return false
		}
		found = list.Items[0]
		return true
	}, "expected exactly one AgentSession to exist")
	return found
}

// waitForTurnContaining polls the session's durable memory transcript for any
// turn whose text contains want, per this harness's convention of asserting
// against memory rather than ExpectAgentReply (which reads the kind:fake
// channel driver — inapplicable here, since this scenario drives the REAL
// slack kind).
func waitForTurnContaining(t *testing.T, h *e2e.Harness, ns, name, want string, timeout time.Duration) {
	t.Helper()
	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + name}
	readCtx := pkgmemory.WithSystemApproval(context.Background(), "e2e-test")
	var dump []pkgmemory.Turn
	e2e.Eventually(t, timeout, func() bool {
		turns, err := turn.ReadAll(readCtx, h.Memory(), scope)
		if err != nil {
			return false
		}
		dump = turns
		for _, tn := range turns {
			if strings.Contains(e2e.FirstText(tn.Content), want) {
				return true
			}
		}
		return false
	}, "no turn contains "+want)
	if t.Failed() {
		t.Logf("transcript:\n%s", e2e.DumpTurns(dump))
	}
}

// nonceFrom extracts the nonce="..." value from an untrusted marker (built
// with %q, e.g. `<untrusted-attachment nonce="abcd1234" …>`). Returns "" when
// the marker carries none, which the caller asserts against — an unbracketed
// native block is the failure this scenario exists to detect.
func nonceFrom(marker string) string {
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

// TestInboundAttachment_NativeImageReachesProviderBracketed drives a PNG from
// a Slack DM all the way to the provider request and asserts on what the
// provider actually received: the file's own bytes, in a native image block,
// wrapped in a matched pair of untrusted-attachment markers, with no
// attachment reference left anywhere in the request.
//
// The bracketing is not cosmetic. A native block lands in a user-role
// message — the most trusted position in the whole conversation — so an image
// carrying "ignore previous instructions" would otherwise be
// indistinguishable from words the human typed. Binary content cannot be
// wrapped inline the way tool output is, so the markers are sibling text
// blocks either side of it, and their nonces must match or they bound
// nothing.
func TestInboundAttachment_NativeImageReachesProviderBracketed(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: agentDir})
	h.WaitForAgentClassValid(agentCls, 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	h.SlackFake().SeedUser(&slackapi.User{
		ID:      humanUserID,
		TeamID:  "T-test",
		Profile: slackapi.UserProfile{Email: humanEmail},
	})

	// THE opt-in. Nothing else in this scenario — not the AgentClass, not the
	// Channel, not the capability grant — says anything about image types;
	// the model registry alone decides, and here the scripted provider stands
	// in for a model row that declares PNG.
	h.LLM.SetNativeInputMIMEs(llm.NewMIMESet(map[string]string{pngMIME: llm.NativeBlockImage}))

	// The same store the runner's hydration pass reads through
	// (Loop.ArtifactReader = files.StoreReader{Store: …}).
	store := blobstore.NewMem()
	h.SetArtifactStore(store)

	// Stands in for the operator's real POST /inbound-asset route. It stores
	// the body it was actually handed rather than a pre-seeded copy, so the
	// byte assertion at the end proves the file travelled the whole path
	// (fakeslack → real slack listener → channelsd → store → hydration →
	// provider) rather than merely that the test can read its own fixture
	// back.
	//
	// Unsupported:true is the honest result for an image: bytes are stored,
	// no extractor claims the MIME, so no TextRef exists. That is exactly the
	// shape whose lifetime rule keeps it attached as a native block — nothing
	// else in the conversation represents it.
	//
	// This callback runs on a channelsd pipeline goroutine, never this test's,
	// so it only records observations under a mutex/atomic; every assertion on
	// them runs in the test body, after the waits that prove it has fired.
	// Calling assert.* here would risk a t.Errorf after the test completes,
	// which panics and masks the real failure.
	var uploadCalls int32
	var mu sync.Mutex
	var gotMIME, gotFilename string
	h.SetInboundAssetUploader(func(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error) {
		atomic.AddInt32(&uploadCalls, 1)
		mu.Lock()
		gotMIME, gotFilename = mime, filename
		mu.Unlock()
		data, err := io.ReadAll(body)
		if err != nil {
			return pipeline.InboundAssetResult{}, err
		}
		ref, err := store.Put(ctx, "inbound/"+filename, bytes.NewReader(data))
		if err != nil {
			return pipeline.InboundAssetResult{}, err
		}
		return pipeline.InboundAssetResult{Ref: string(ref), Unsupported: true}, nil
	})

	file := h.SlackFake().SeedFile("F1", pngFilename, pngMIME, pngBytes)

	// "was attached" is the agent-facing note channelsd writes for a stored
	// attachment with no extracted text — deliberately NOT "cannot be read",
	// because whether it can is a send-time question this scenario answers
	// with a native block.
	h.LLM.OnUserMessage("was attached").
		Reply(e2e.RespondToUser("I can see the screenshot."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendSlackDMWithFiles(humanUserID, "D-native-attach", "what do you make of this?", []slackapi.File{file})

	sess := waitForSingleSession(t, h, 20*time.Second)
	waitForTurnContaining(t, h, sess.Namespace, sess.Name, "was attached", 20*time.Second)
	waitForTurnContaining(t, h, sess.Namespace, sess.Name, "I can see the screenshot.", 20*time.Second)

	// Settle before sampling the provider. The assistant turn asserted above
	// is durable BEFORE its respond_to_user tool result comes back, so it is
	// not a settle point: it can be observed while the second LLM request —
	// and therefore the second rule — is still in flight, and every sample
	// below (Requests, AssertAllRulesConsumed) would then read a conversation
	// mid-turn. Idle is the settle point that proves the loop finished; see
	// ScriptedLLM.AssertAllRulesConsumed's own doc.
	e2e.WaitForSessionIdle(t, h)

	assert.Equal(t, int32(1), atomic.LoadInt32(&uploadCalls), "exactly one attachment must have been uploaded")
	mu.Lock()
	assert.Equal(t, pngMIME, gotMIME)
	assert.Equal(t, pngFilename, gotFilename)
	mu.Unlock()

	// What the provider actually received.
	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "the scripted provider must have served at least one request")

	nativeBlocks := 0
	refBlocks := 0
	for r, req := range reqs {
		for _, m := range req.Messages {
			for i, b := range m.Content {
				if b.Type == "attachment" || b.Attachment != nil {
					refBlocks++
					continue
				}
				if b.Type != llm.NativeBlockImage {
					continue
				}
				nativeBlocks++

				// The bracketing this test exists to prove is only necessary
				// because the block lands in a user-role message — the most
				// trusted position in the conversation. Nothing else pins that,
				// so a future change that delivered attachments in some other
				// role would quietly make the markers ceremonial and this
				// scenario would still pass.
				assert.Equal(t, "user", m.Role,
					"request %d: a native block must land in a user-role message — that position is why bracketing matters at all", r)
				assert.Equal(t, pngMIME, b.MIME, "request %d: the native block must carry the declared MIME", r)
				assert.Equal(t, pngBytes, b.Data,
					"request %d: the native block must carry the bytes the user actually uploaded, unaltered end to end", r)

				require.Greater(t, i, 0, "request %d: a native block must be preceded by an opening untrusted marker", r)
				require.Less(t, i, len(m.Content)-1, "request %d: a native block must be followed by a closing untrusted marker", r)
				open, closing := m.Content[i-1].Text, m.Content[i+1].Text
				// Assert the OPEN and CLOSE forms separately: a marker missing
				// its "/" would still satisfy a bare Contains(tag) on both
				// sides while terminating nothing.
				assert.True(t, strings.HasPrefix(open, "<"+untrusted.AttachmentTag),
					"request %d: opening marker must start with <%s, got %q", r, untrusted.AttachmentTag, open)
				assert.True(t, strings.Contains(closing, "</"+untrusted.AttachmentTag),
					"request %d: closing marker must contain </%s, got %q", r, untrusted.AttachmentTag, closing)
				assert.Contains(t, open, pngFilename, "request %d: the opening marker names the file it wraps", r)
				nonce := nonceFrom(open)
				assert.NotEmpty(t, nonce, "request %d: opening marker must carry a nonce", r)
				assert.Equal(t, nonce, nonceFrom(closing),
					"request %d: the closing nonce must match its own opening one, or the pair bounds nothing", r)
			}
		}
	}
	assert.Positive(t, nativeBlocks,
		"the uploaded image must reach the provider as a native block — declaring its MIME on the model row is the whole opt-in")
	assert.Zero(t, refBlocks,
		"no attachment reference may reach a provider adapter: every adapter hard-errors on an unknown block type")

	h.AssertAllRulesConsumed()
}
