//go:build e2e

// Package attachments_test is Task 7's payoff: the first test that drives
// the inbound-attachments feature through its ENTIRE path in one process —
// the real slack channel kind's listener (Task 4) → channelsd's gate +
// notices + turn manifest (Task 5/6) → the runner's fetch_artifact tool,
// wired here for the first time in the e2e harness — exactly the kind of
// cross-component seam prior rounds of this feature broke on (a Slack
// subtype gate that dropped file messages, a storage key layout that broke
// fetch_artifact's session-token auth, an operator size cap contradicting
// the transport's, a turn block type nothing could render). Each component
// already has its own unit tests; this scenario exists to catch what only
// shows up when they're wired together — and did: the gate-open fixture
// grants ONLY the "attachments" capability (no "artifacts"), which is what
// first caught fetch_artifact being unreachable under that exact,
// documented configuration until pkg/agent/tool/meta/capability's C1 fix
// (attachmentsCapability.Offer now injects it directly — see that file).
//
// The harness's in-process channelsd pipeline (test/e2e/harness.go) has no
// artifactstore or extractord of its own, by design — production storage +
// extraction happen over HTTP against the operator (POST
// /inbound-asset/{ns}/{sess}, which the operator's own extractordclient DI
// wires in internal/cmd/operator/main.go). This scenario stands in for that HTTP hop
// with Harness.SetInboundAssetUploader (a fixture-controlled
// UploadInboundAsset implementation) and Harness.SetArtifactStore (the SAME
// store fetch_artifact's Tier-1 reader reads from) — both new e2e seams
// added by this task, mirroring the existing SetMinter/SetContentInspectors
// pattern. What they do NOT re-test is internal/cmd/operator's own extractordclient
// wiring or the real internal/cmd/extractord binary; those are covered by
// pkg/platform/extract/extractordclient's unit tests and code review.
//
// No real names — "user@example.com" is fictional per AGENTS.md, and is the
// same default identity test/e2e's Options.DefaultUser and slack_dm_threading
// use.
package attachments_test

import (
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

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	gateOpenAgentDir   = "../../testdata/agent-attachments-gate-open"
	gateOpenAgentCls   = "attach-open-agent"
	gateClosedAgentDir = "../../testdata/agent-attachments-gate-closed"
	gateClosedAgentCls = "attach-closed-agent"

	// humanUserID/humanEmail are the seeded sender identity. TeamID MUST
	// equal fakeslack's AuthTestContext TeamID ("T-test") — the real
	// listener's emailTrusted gate only honors the profile email for a full
	// member of the bot's own installed workspace.
	humanUserID = "U-attach-human"
	humanEmail  = "user@example.com"

	docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func seedHumanUser(h *e2e.Harness) {
	h.SlackFake().SeedUser(&slackapi.User{
		ID:     humanUserID,
		TeamID: "T-test",
		Profile: slackapi.UserProfile{
			Email: humanEmail,
		},
	})
}

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

// waitForTurnContaining polls the session's durable memory transcript for
// any turn whose text contains want, per this harness's convention of
// asserting against memory rather than ExpectAgentReply (which reads the
// kind:fake channel driver — inapplicable here, since this scenario drives
// the REAL slack kind).
func waitForTurnContaining(t *testing.T, h *e2e.Harness, ns, name, want string, timeout time.Duration) []pkgmemory.Turn {
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
	return dump
}

// TestAttachments_GateOpen_AgentReadsExtractedTextViaFetchArtifact drives a
// Slack DM carrying a DOCX attachment through the real slack listener,
// proves channelsd's gate-open path stores it and records a turn manifest
// naming a fetch_artifact handle, and proves the runner's fetch_artifact
// tool actually resolves that handle to the extracted text — the payoff
// this whole feature exists for.
func TestAttachments_GateOpen_AgentReadsExtractedTextViaFetchArtifact(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: gateOpenAgentDir})
	h.WaitForAgentClassValid(gateOpenAgentCls, 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	seedHumanUser(h)

	// Stands in for the operator's real POST /inbound-asset route: stores
	// the extracted text at a deterministic ref this test controls, so the
	// ScriptedLLM's fetch_artifact call below can name it exactly.
	store := blobstore.NewMem()
	h.SetArtifactStore(store)

	const extractedText = "quarterly revenue grew eleven percent across every region"
	textRef, err := store.Put(context.Background(), "attach-fixture/text.txt", strings.NewReader(extractedText))
	require.NoError(t, err)

	// uploadCalls/gotMIME/gotFilename are written from the callback below,
	// which runs on a channelsd pipeline goroutine, not this test's own
	// goroutine — assert.* must NEVER be called from there: a late/retried
	// invocation racing this test's completion would call t.Errorf from a
	// goroutine after the test has already finished, which panics as "Log in
	// goroutine after Test has completed" and reads as a harness crash,
	// masking whatever the real assertion failure was. The callback only
	// records observations (atomic/mutex-guarded); every assert.* on these
	// values runs in the test body below, after the waits that already prove
	// the callback has fired.
	var uploadCalls int32
	var mu sync.Mutex
	var gotMIME, gotFilename string
	h.SetInboundAssetUploader(func(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error) {
		atomic.AddInt32(&uploadCalls, 1)
		mu.Lock()
		gotMIME, gotFilename = mime, filename
		mu.Unlock()
		if body != nil {
			_, _ = io.Copy(io.Discard, body) // drain, mirroring the real route's "always read to EOF"
		}
		return pipeline.InboundAssetResult{
			Ref: "raw-attach-fixture", Extracted: true, TextRef: string(textRef), Pages: 3,
		}, nil
	})

	file := h.SlackFake().SeedFile("F1", "quarterly-report.docx", docxMIME, []byte("fake raw docx bytes"))

	h.LLM.OnUserMessage("was attached and read").
		Reply(e2e.ToolUse("fetch_artifact", map[string]any{"handle": string(textRef)}))
	h.LLM.OnToolResult("fetch_artifact", e2e.ResultContains(extractedText)).
		Reply(e2e.RespondToUser("Read the attachment: " + extractedText))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendSlackDMWithFiles(humanUserID, "D-attach-open", "check this out", []slackapi.File{file})

	sess := waitForSingleSession(t, h, 20*time.Second)

	// Channelsd-side proof: the inbound turn's manifest line named this
	// exact fetch_artifact handle.
	waitForTurnContaining(t, h, sess.Namespace, sess.Name,
		"was attached and read", 20*time.Second)
	waitForTurnContaining(t, h, sess.Namespace, sess.Name,
		string(textRef), 20*time.Second)

	// Runner-side proof: fetch_artifact actually resolved that handle to the
	// real extracted text, and the agent's reply (also durably recorded)
	// carries it.
	waitForTurnContaining(t, h, sess.Namespace, sess.Name, extractedText, 20*time.Second)

	// Same settle point as the gate-closed sibling, for the narrower reason:
	// the reply turn above lands when respond_to_user EXECUTES, one LLM
	// round-trip before the closing end_turn rule is served. Asserting rules
	// consumed off the reply alone races that last round-trip.
	e2e.WaitForSessionIdle(t, h)

	assert.Equal(t, int32(1), atomic.LoadInt32(&uploadCalls), "exactly one attachment must have been uploaded")
	mu.Lock()
	assert.Equal(t, docxMIME, gotMIME)
	assert.Equal(t, "quarterly-report.docx", gotFilename)
	mu.Unlock()
	h.AssertAllRulesConsumed()
}

// TestAttachments_GateClosed_NoticeRecordedAndNoArtifactStored proves the
// gate-closed path end to end through the real slack listener: with the
// AgentClass granting neither "attachments" nor "artifacts" (the Channel
// itself stays opted in — see the gate-attachments-gate-closed fixture's
// header), the agent's turn carries the disabled-gate notice and — the
// load-bearing assertion — UploadInboundAsset is never called at all, so
// nothing is ever stored.
func TestAttachments_GateClosed_NoticeRecordedAndNoArtifactStored(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: gateClosedAgentDir})
	h.WaitForAgentClassValid(gateClosedAgentCls, 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	seedHumanUser(h)

	var uploadCalls int32
	h.SetInboundAssetUploader(func(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error) {
		atomic.AddInt32(&uploadCalls, 1)
		if body != nil {
			_, _ = io.Copy(io.Discard, body)
		}
		return pipeline.InboundAssetResult{}, nil
	})

	file := h.SlackFake().SeedFile("F1", "quarterly-report.docx", docxMIME, []byte("fake raw docx bytes"))

	h.LLM.OnUserMessage("cannot read attachments").
		Reply(e2e.RespondToUser("Got your message, but I can't read files here."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendSlackDMWithFiles(humanUserID, "D-attach-closed", "check this out", []slackapi.File{file})

	sess := waitForSingleSession(t, h, 20*time.Second)
	waitForTurnContaining(t, h, sess.Namespace, sess.Name, "cannot read attachments", 20*time.Second)

	// The note turn is written by CHANNELSD, before the runner for this
	// session exists at all — so it proves nothing about the scripted
	// conversation and cannot gate AssertAllRulesConsumed below. Idle is the
	// runner-side settle point: it is only reached once the turn loop has run
	// to completion, which is exactly when every scripted rule (including the
	// closing end_turn round-trip) has been served.
	e2e.WaitForSessionIdle(t, h)

	assert.Equal(t, int32(0), atomic.LoadInt32(&uploadCalls),
		"gate-closed must never call UploadInboundAsset — no bytes leave the pipeline, let alone get stored")
	h.AssertAllRulesConsumed()
}
