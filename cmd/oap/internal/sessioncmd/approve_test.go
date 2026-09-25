package sessioncmd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliidentity"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeLookuper satisfies subjectLookuper for the approver pre-check tests.
// bySubject, when non-nil, answers per subject-ref (multi-set tests);
// otherwise subjects/err apply to every lookup.
type fakeLookuper struct {
	subjects  []string
	bySubject map[string][]string
	err       error
}

func (f fakeLookuper) LookupSubjects(_ context.Context, ref string) ([]string, error) {
	if f.bySubject != nil {
		return f.bySubject[ref], f.err
	}
	return f.subjects, f.err
}

func TestApproverAuthorized(t *testing.T) {
	ctx := context.Background()

	t.Run("authorized: approver is in the subject set", func(t *testing.T) {
		ok, authorized, err := approverAuthorized(ctx,
			fakeLookuper{subjects: []string{"alice-canon", "bob-canon"}},
			[]string{"crm_company:54835545860#owner"}, identity.CanonicalFromTrusted("alice-canon", "test fixture"))
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, []string{"alice-canon", "bob-canon"}, authorized,
			"the full authorized list is returned for display")
	})

	t.Run("not authorized: approver absent → ok=false, list still returned", func(t *testing.T) {
		ok, authorized, err := approverAuthorized(ctx,
			fakeLookuper{subjects: []string{"alice-canon"}},
			[]string{"crm_company:54835545860#owner"}, identity.CanonicalFromTrusted("cody-canon", "test fixture"))
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, []string{"alice-canon"}, authorized)
	})

	t.Run("direct user subject: LookupSubjects collapses to that user", func(t *testing.T) {
		ok, _, err := approverAuthorized(ctx,
			fakeLookuper{subjects: []string{"alice-canon"}},
			[]string{"user:alice-canon"}, identity.CanonicalFromTrusted("alice-canon", "test fixture"))
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("multi-set union: member of ANY set is authorized, list deduped", func(t *testing.T) {
		ok, authorized, err := approverAuthorized(ctx,
			fakeLookuper{bySubject: map[string][]string{
				"issue:ENG-1#owner": {"alice-canon", "bob-canon"},
				"issue:ENG-2#owner": {"bob-canon", "carol-canon"},
			}},
			[]string{"issue:ENG-1#owner", "issue:ENG-2#owner"}, identity.CanonicalFromTrusted("carol-canon", "test fixture"))
		require.NoError(t, err)
		assert.True(t, ok, "carol owns ENG-2 only — union semantics authorize her")
		assert.Equal(t, []string{"alice-canon", "bob-canon", "carol-canon"}, authorized,
			"authorized list is the deduped union")
	})

	t.Run("lookup error: ok=false, error propagated (caller warns + proceeds)", func(t *testing.T) {
		ok, _, err := approverAuthorized(ctx,
			fakeLookuper{err: errors.New("spicedb unreachable")},
			[]string{"crm_company:54835545860#owner"}, identity.CanonicalFromTrusted("alice-canon", "test fixture"))
		require.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("empty set: ok=false, no error", func(t *testing.T) {
		ok, authorized, err := approverAuthorized(ctx,
			fakeLookuper{subjects: nil},
			[]string{"crm_company:54835545860#owner"}, identity.CanonicalFromTrusted("alice-canon", "test fixture"))
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, authorized)
	})
}

// TestSessionApprove_CanonicalResolution verifies the two canonicalization
// paths in runSessionApprove's approver derivation:
//
//  1. --approver-email flag set → identity.EmailReference(email).Canonical()
//  2. No flag → clilogin.EnsureIdentity cache-hit → VerifiedEmail(email).Canonical(),
//     which must equal EmailReference(email).Canonical() (same canonical,
//     different EmailVerified).
//
// These tests drive the derivation logic directly (outside the full
// runSessionApprove stack which requires NATS + cluster) to pin the
// canonical contract that channelsd relies on for the approver pre-check.
func TestSessionApprove_CanonicalResolution(t *testing.T) {
	t.Run("flag set: EmailReference canonical matches EmailReference output", func(t *testing.T) {
		// The existing path: EmailReference(email) is canonical source of truth.
		email := "alice@example.com"
		wantCanonical, err := identity.EmailReference(identity.Email(email)).Canonical()
		require.NoError(t, err)
		// The same canonical must come from a VerifiedEmail principal (cache
		// hit) so a user who has logged in gets the same SpiceDB subject as
		// one who passes --approver-email directly.
		gotCanonical, err := identity.VerifiedEmail(identity.Email(email), "Alice").Canonical()
		require.NoError(t, err)
		assert.Equal(t, wantCanonical, gotCanonical,
			"VerifiedEmail and EmailReference must produce the same canonical for the same address")
	})

	t.Run("no flag + cached assertion: clilogin.EnsureIdentity returns canonical matching EmailReference", func(t *testing.T) {
		// Override cliidentity to a temp dir and seed it with a valid cache.
		aptest.FakeIdentityConfigDir(t)
		require.NoError(t, cliidentity.Save(cliidentity.Cached{
			Email:       "bob@example.com",
			DisplayName: "Bob",
			ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
		}))

		// Simulate the no-flag path: clilogin.EnsureIdentity cache-hit → p.Canonical().
		// We use ensureCLIIdentityWithBundle (the test helper from login_test.go)
		// with an empty fake client (no ClusterIdentityProvider CR) so it
		// hits the cache-hit branch immediately.
		p, err := clilogin.EnsureIdentityWithClient(context.Background(), aptest.NewClient(t))
		require.NoError(t, err)
		require.True(t, p.EmailVerified(), "cache-hit principal must be email-verified")

		// The canonical from the cache-hit principal must equal the canonical
		// that runSessionApprove would produce if --approver-email were passed.
		wantCanonical, err := identity.EmailReference("bob@example.com").Canonical()
		require.NoError(t, err)
		gotCanonical, err := p.Canonical()
		require.NoError(t, err)
		assert.Equal(t, wantCanonical, gotCanonical,
			"approver canonical from cached identity must match EmailReference canonical for the same address")
	})
}

// sessionWith builds an AgentSession whose status carries the given generic
// PendingInteractions entries. Helper for the unified-resolution tests. Since
// Slice C2 all three approval families (tool_approval, info_leakage,
// content_inspection) park on this single list; collectPendings maps each
// entry's Category onto its CLI approvalKind and skips categories the command
// does not drive.
func sessionWith(pi ...spiceboxv1alpha1.PendingInteraction) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Status.PendingInteractions = pi
	return s
}

// TestCollectPendings verifies the generic PendingInteractions list flattens
// into one ordered view with the right kind tag per Category, and that a
// non-approval category (identity_choice) is skipped.
func TestCollectPendings(t *testing.T) {
	sess := sessionWith(
		spiceboxv1alpha1.PendingInteraction{RequestID: "t1", Category: categories.ToolApproval, ApproverSubject: "crm_company:1#owner"},
		spiceboxv1alpha1.PendingInteraction{RequestID: "l1", Category: categories.InfoLeakage, ApproverSubject: "crm_company:2#owner"},
		spiceboxv1alpha1.PendingInteraction{RequestID: "c1", Category: categories.ContentInspection, ApproverSubject: "crm_company:3#owner"},
		spiceboxv1alpha1.PendingInteraction{RequestID: "x1", Category: categories.IdentityChoice, ApproverSubject: "crm_company:4#owner"},
	)
	got := collectPendings(sess)
	require.Len(t, got, 3, "identity_choice is not an approval family this command drives")
	assert.Equal(t, pendingEntry{RequestID: "t1", ApproverSubject: "crm_company:1#owner", Kind: approvalKindTool}, got[0])
	assert.Equal(t, pendingEntry{RequestID: "l1", ApproverSubject: "crm_company:2#owner", Kind: approvalKindLeakage}, got[1])
	assert.Equal(t, pendingEntry{RequestID: "c1", ApproverSubject: "crm_company:3#owner", Kind: approvalKindContentInspection}, got[2])
}

// TestResolvePending covers the unified resolution: auto-pick (one only),
// disambiguation error (multiple), no-pending error, explicit match, and
// explicit no-match (across all three kinds).
func TestResolvePending(t *testing.T) {
	tool := pendingEntry{RequestID: "t1", ApproverSubject: "subj-t", Kind: approvalKindTool}
	leak := pendingEntry{RequestID: "l1", ApproverSubject: "subj-l", Kind: approvalKindLeakage}
	ci := pendingEntry{RequestID: "c1", ApproverSubject: "subj-c", Kind: approvalKindContentInspection}

	t.Run("auto-pick: single content_inspection pending → that entry", func(t *testing.T) {
		got, err := resolvePending([]pendingEntry{ci}, "", "ns", "sess")
		require.NoError(t, err)
		assert.Equal(t, ci, got)
	})

	t.Run("auto-pick: single leakage pending → that entry", func(t *testing.T) {
		got, err := resolvePending([]pendingEntry{leak}, "", "ns", "sess")
		require.NoError(t, err)
		assert.Equal(t, leak, got)
	})

	t.Run("empty + none pending → no-pending error", func(t *testing.T) {
		_, err := resolvePending(nil, "", "ns", "sess")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no pending approvals on AgentSession ns/sess")
	})

	t.Run("empty + multiple → error lists every id with its kind", func(t *testing.T) {
		_, err := resolvePending([]pendingEntry{tool, leak, ci}, "", "ns", "sess")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple pending approvals")
		assert.Contains(t, err.Error(), "t1 (tool_call)")
		assert.Contains(t, err.Error(), "l1 (leakage_share)")
		assert.Contains(t, err.Error(), "c1 (content_inspection)")
	})

	t.Run("explicit match against the union → matched entry (any kind)", func(t *testing.T) {
		got, err := resolvePending([]pendingEntry{tool, leak, ci}, "c1", "ns", "sess")
		require.NoError(t, err)
		assert.Equal(t, ci, got)
	})

	t.Run("explicit no-match → error lists all pending with kinds", func(t *testing.T) {
		_, err := resolvePending([]pendingEntry{tool, leak, ci}, "nope", "ns", "sess")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `requestID "nope" is not pending`)
		assert.Contains(t, err.Error(), "t1 (tool_call)")
		assert.Contains(t, err.Error(), "l1 (leakage_share)")
		assert.Contains(t, err.Error(), "c1 (content_inspection)")
	})
}

// capturePublish records the single subject + envelope a build+publish
// produces, so the per-kind decision-envelope tests can assert the wire
// shape without standing up NATS. It mirrors the production publish path:
// dispatch.buildDecisionPayload → channelevents.PublishIn.
func capturePublish(t *testing.T, k approvalKind, requestID, approverEmail, decision string) channelevents.Envelope {
	t.Helper()
	dispatch, ok := dispatchFor(k)
	require.True(t, ok, "dispatchFor(%q) must be known", k)

	var gotSubject string
	var gotEnv channelevents.Envelope
	publish := channelevents.PublishFunc(func(subject string, body []byte) error {
		gotSubject = subject
		require.NoError(t, json.Unmarshal(body, &gotEnv), "envelope must unmarshal")
		return nil
	})
	pl := dispatch.buildDecisionPayload(requestID, approverEmail, decision)
	require.NoError(t, channelevents.PublishIn(publish, "ns", "sess", dispatch.decisionKind, pl))

	// IN subject for the kind's decision envelope.
	wantSubject := channelevents.SubjectIn(channelevents.SubjectPrefix("ns", "sess"), dispatch.decisionKind)
	assert.Equal(t, wantSubject, gotSubject)
	require.NoError(t, gotEnv.Validate(), "published envelope must validate")
	return gotEnv
}

// TestBuildDecisionPayload_PerKind verifies each kind's auto-picked
// approve/deny publishes the correct decision Kind + a payload carrying
// the requestID, the "cli" approver, and the decision string.
func TestBuildDecisionPayload_PerKind(t *testing.T) {
	t.Run("content_inspection approve: generic KindInteractionDecision envelope (C1: driven off PendingInteractions)", func(t *testing.T) {
		env := capturePublish(t, approvalKindContentInspection, "c1", "alice@example.com", "approve")
		assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)

		var pl channelevents.InteractionDecisionPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.Equal(t, "content_inspection", pl.Category)
		assert.Equal(t, "c1", pl.RequestRef)
		assert.Equal(t, "approve", pl.ActionID)
		assert.Equal(t, "cli", pl.Decider.Kind.String())
		assert.Equal(t, "alice@example.com", pl.Decider.Email.String())
		// InteractionDecisionPayload.Validate requires a non-empty
		// Decider.ExternalID; the cli approver keys on Email for
		// canonicalization, so dispatchFor's content_inspection arm must mirror
		// the email into ExternalID or this (and the server's Validate) fails.
		assert.Equal(t, "alice@example.com", pl.Decider.ExternalID.String())
		assert.NoError(t, pl.Validate(), "decider must satisfy InteractionDecisionPayload.Validate")
	})

	t.Run("content_inspection deny: ActionID flips to deny", func(t *testing.T) {
		env := capturePublish(t, approvalKindContentInspection, "c1", "alice@example.com", "deny")
		assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)
		var pl channelevents.InteractionDecisionPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.Equal(t, "deny", pl.ActionID)
		assert.Equal(t, "alice@example.com", pl.Decider.ExternalID.String())
	})

	t.Run("leakage approve: generic KindInteractionDecision envelope (Category=info_leakage)", func(t *testing.T) {
		env := capturePublish(t, approvalKindLeakage, "l1", "bob@example.com", "approve")
		assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)
		var pl channelevents.InteractionDecisionPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.Equal(t, string(categories.InfoLeakage), pl.Category)
		assert.Equal(t, "l1", pl.RequestRef)
		assert.Equal(t, "approve", pl.ActionID)
		assert.Equal(t, "cli", pl.Decider.Kind.String())
		assert.Equal(t, "bob@example.com", pl.Decider.Email.String())
		assert.Equal(t, "bob@example.com", pl.Decider.ExternalID.String())
		assert.NoError(t, pl.Validate(), "decider must satisfy InteractionDecisionPayload.Validate")
	})

	t.Run("tool_call approve: generic KindInteractionDecision envelope (Category=tool_approval)", func(t *testing.T) {
		env := capturePublish(t, approvalKindTool, "t1", "alice@example.com", "approve")
		assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)
		var pl channelevents.InteractionDecisionPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.Equal(t, string(categories.ToolApproval), pl.Category)
		assert.Equal(t, "t1", pl.RequestRef)
		assert.Equal(t, "approve", pl.ActionID)
		assert.Equal(t, "cli", pl.Decider.Kind.String())
		assert.Equal(t, "alice@example.com", pl.Decider.ExternalID.String())
		assert.NoError(t, pl.Validate(), "decider must satisfy InteractionDecisionPayload.Validate")
	})
}

// TestDecodeApplied_PerKind verifies the --wait Applied-decode normalizes
// each kind's distinct payload shape onto a uniform appliedOutcome, and
// that a mismatched requestID is ignored.
func TestDecodeApplied_PerKind(t *testing.T) {
	t.Run("content_inspection: generic Outcome=approved → approve, DecidedBy surfaced as kind:externalId", func(t *testing.T) {
		d, _ := dispatchFor(approvalKindContentInspection)
		body, err := json.Marshal(channelevents.InteractionAppliedPayload{
			Category:   "content_inspection",
			RequestRef: "c1",
			Outcome:    channelevents.OutcomeApproved,
			DecidedBy:  &channelevents.ExternalIdentity{Kind: "cli", ExternalID: "alice@example.com"},
		})
		require.NoError(t, err)
		got, ok := d.decodeApplied(body, "c1")
		require.True(t, ok)
		assert.Equal(t, "approve", got.Decision)
		assert.Equal(t, "cli:alice@example.com", got.ApproverDisplay)
	})

	t.Run("content_inspection deny: generic Outcome=denied → deny", func(t *testing.T) {
		d, _ := dispatchFor(approvalKindContentInspection)
		body, _ := json.Marshal(channelevents.InteractionAppliedPayload{
			Category:   "content_inspection",
			RequestRef: "c1",
			Outcome:    channelevents.OutcomeDenied,
			DecidedBy:  &channelevents.ExternalIdentity{Kind: "cli", ExternalID: "alice@example.com"},
		})
		got, ok := d.decodeApplied(body, "c1")
		require.True(t, ok)
		assert.Equal(t, "deny", got.Decision)
	})

	t.Run("leakage: generic Outcome=denied + not_authorized reason threads through", func(t *testing.T) {
		d, _ := dispatchFor(approvalKindLeakage)
		body, _ := json.Marshal(channelevents.InteractionAppliedPayload{
			Category: string(categories.InfoLeakage), RequestRef: "l1",
			Outcome: channelevents.OutcomeDenied, Reason: "not_authorized",
			DecidedBy: &channelevents.ExternalIdentity{Kind: "cli", ExternalID: "bob"},
		})
		got, ok := d.decodeApplied(body, "l1")
		require.True(t, ok)
		assert.Equal(t, "deny", got.Decision)
		assert.Equal(t, "not_authorized", got.Reason)
		assert.Equal(t, "cli:bob", got.ApproverDisplay)
	})

	t.Run("tool_call: generic Outcome=approved + DecidedBy surfaced as kind:externalId", func(t *testing.T) {
		d, _ := dispatchFor(approvalKindTool)
		body, _ := json.Marshal(channelevents.InteractionAppliedPayload{
			Category: string(categories.ToolApproval), RequestRef: "t1",
			Outcome:   channelevents.OutcomeApproved,
			DecidedBy: &channelevents.ExternalIdentity{Kind: "cli", ExternalID: "alice"},
		})
		got, ok := d.decodeApplied(body, "t1")
		require.True(t, ok)
		assert.Equal(t, "approve", got.Decision)
		assert.Equal(t, "cli:alice", got.ApproverDisplay)
	})

	t.Run("mismatched requestID is ignored", func(t *testing.T) {
		d, _ := dispatchFor(approvalKindContentInspection)
		body, _ := json.Marshal(channelevents.InteractionAppliedPayload{
			Category: "content_inspection", RequestRef: "other", Outcome: channelevents.OutcomeApproved,
		})
		_, ok := d.decodeApplied(body, "c1")
		assert.False(t, ok)
	})
}
