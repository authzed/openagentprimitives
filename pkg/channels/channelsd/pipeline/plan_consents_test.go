package pipeline

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type planConsentCommitter struct {
	keys      provenance.MapKeyLookup
	decisions map[string]memory.Entry
	failID    string
}

func (c *planConsentCommitter) CommitGoalConsent(_ context.Context, entry memory.Entry) error {
	if err := provenance.VerifyEntrySignature(c.keys, entry); err != nil {
		return err
	}
	if entry.ID == c.failID {
		return errors.New("grant write failed")
	}
	c.decisions[entry.ID] = entry
	return nil
}

func signedPlanConsents(t *testing.T) (*Pipeline, *planConsentCommitter, channelevents.InteractionRequestPayload, context.Context) {
	t.Helper()
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)
	keys := provenance.MapKeyLookup{}
	newSigner := func(publisher string) *provenance.Signer {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		signer := provenance.NewSigner(priv, publisher)
		keys[provenance.PubKeyRef{Publisher: publisher, KeyID: signer.KeyID()}] = pub
		return signer
	}
	operator, channels := newSigner("system:operator"), newSigner("system:channelsd")
	mem := memory.NewLocal(inmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	committer := &planConsentCommitter{keys: keys, decisions: map[string]memory.Entry{}}
	p := &Pipeline{Mem: provenance.NewSigningMemory(mem, channels), GoalConsentCommitter: committer}
	BindGoalConsentHandler(p)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	ref := channelevents.SessionRef{Namespace: "team", Name: "source"}
	owner := channelevents.ExternalIdentity{Kind: "email", ExternalID: "alice@example.com", Email: "alice@example.com"}
	expires := time.Now().UTC().Add(time.Hour)
	parent := channelevents.InteractionRequestPayload{AgentSessionRef: ref, Category: categories.PlanPhase, RequestRef: "plan", Lead: "Approve the plan and its two reminders", ExpiresAt: &expires, Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: []channelevents.ExternalIdentity{owner}}, Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}, {ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision}}}
	for _, id := range []string{"first", "second"} {
		child := channelevents.InteractionRequestPayload{AgentSessionRef: ref, Category: categories.GoalExecutionConsent, RequestRef: "goalconsent-request-" + id, Lead: "Authorize one private reminder", Fields: []channelevents.InteractionField{{Label: "Run once", Value: "2030-01-01T12:00:00Z"}, {Label: "Private recipient", Value: "alice@example.com"}}, Excerpt: &channelevents.InteractionExcerpt{Label: "Goal", Content: "Stand up and stretch"}, ExpiresAt: &expires, Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &owner}, Actions: parent.Actions}
		raw, err := json.Marshal(child)
		require.NoError(t, err)
		content, err := json.Marshal(goalconsent.Content{Goal: goals.Goal{ID: id, Domain: goals.Domain{Namespace: "team", Owner: "YWxpY2VAZXhhbXBsZS5jb20", Class: "assistant", ClassUID: "class"}, Execution: &goals.ExecutionConsent{Session: "team/source", SessionUID: "source-uid", Digest: id, ApprovalMode: "plan"}}, Request: raw})
		require.NoError(t, err)
		_, err = provenance.NewSigningMemory(mem, operator).Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/source"}, Kind: goalconsent.KindName, ID: child.RequestRef, CreatedAt: time.Now().UTC(), Content: content})
		require.NoError(t, err)
		parent.Consents = append(parent.Consents, child)
	}
	parent.Fields, parent.Excerpt = channelevents.PlanConsentFields(parent.Consents), channelevents.PlanConsentExcerpt(parent.Consents)
	parent.Details, _ = json.Marshal(parent.Consents)
	ctx = channelevents.WithComponentDecisionIngress(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd"))
	return p, committer, parent, ctx
}

func TestComposedPlanConsentsPreflightEverySignedRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*channelevents.InteractionRequestPayload, *channelevents.InteractionDecisionPayload)
	}{
		{"wrong human rejects", func(_ *channelevents.InteractionRequestPayload, d *channelevents.InteractionDecisionPayload) {
			d.Decider.Email = "bob@example.com"
		}},
		{"hidden terms reject", func(r *channelevents.InteractionRequestPayload, _ *channelevents.InteractionDecisionPayload) {
			r.Fields = nil
		}},
		{"rewritten second signed card rejects before first commit", func(r *channelevents.InteractionRequestPayload, _ *channelevents.InteractionDecisionPayload) {
			r.Consents[1].Fields[0].Value = "2030-01-01T13:00:00Z"
			r.Fields = channelevents.PlanConsentFields(r.Consents)
			r.Excerpt = channelevents.PlanConsentExcerpt(r.Consents)
			r.Details, _ = json.Marshal(r.Consents)
		}},
		{"foreign session rejects", func(r *channelevents.InteractionRequestPayload, _ *channelevents.InteractionDecisionPayload) {
			r.Consents[1].AgentSessionRef.Name = "foreign"
		}},
		{"expired parent rejects", func(r *channelevents.InteractionRequestPayload, _ *channelevents.InteractionDecisionPayload) {
			expired := time.Now().UTC().Add(-time.Second)
			r.ExpiresAt = &expired
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c, r, ctx := signedPlanConsents(t)
			d := channelevents.InteractionDecisionPayload{Category: r.Category, RequestRef: r.RequestRef, ActionID: "approve", Decider: *r.Consents[0].Audience.Requester}
			tc.mutate(&r, &d)
			parentApplied := false
			_, err := p.decideWithConsents(ctx, r.AgentSessionRef, d, &r, func(context.Context, channelinteractions.Decision) (channelinteractions.Outcome, error) {
				parentApplied = true
				return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
			})
			require.Error(t, err)
			assert.Empty(t, c.decisions)
			assert.False(t, parentApplied)
		})
	}
}

func TestComposedPlanConsentRetriesPartialGrantFailure(t *testing.T) {
	p, c, r, ctx := signedPlanConsents(t)
	d := channelevents.InteractionDecisionPayload{Category: r.Category, RequestRef: r.RequestRef, ActionID: "approve", Decider: *r.Consents[0].Audience.Requester}
	parentCalls := 0
	handler := func(context.Context, channelinteractions.Decision) (channelinteractions.Outcome, error) {
		parentCalls++
		return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
	}
	c.failID = "goalconsent-decision-second"
	_, err := p.decideWithConsents(ctx, r.AgentSessionRef, d, &r, handler)
	require.ErrorContains(t, err, "grant write failed")
	assert.Len(t, c.decisions, 1)
	assert.Zero(t, parentCalls)
	first := c.decisions["goalconsent-decision-first"]
	c.failID = ""
	out, err := p.decideWithConsents(ctx, r.AgentSessionRef, d, &r, handler)
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)
	assert.Len(t, c.decisions, 2)
	assert.Equal(t, 1, parentCalls)
	assert.Equal(t, first, c.decisions["goalconsent-decision-first"], "retry retains the same signed decision")
}

func TestComposedPlanDenialRecordsNoApprovedChild(t *testing.T) {
	p, c, r, ctx := signedPlanConsents(t)
	d := channelevents.InteractionDecisionPayload{Category: r.Category, RequestRef: r.RequestRef, ActionID: "deny", Decider: *r.Consents[0].Audience.Requester}
	parentCalls := 0
	out, err := p.decideWithConsents(ctx, r.AgentSessionRef, d, &r, func(context.Context, channelinteractions.Decision) (channelinteractions.Outcome, error) {
		parentCalls++
		return channelinteractions.Outcome{Result: channelevents.OutcomeDenied}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeDenied, out.Result)
	assert.Equal(t, 1, parentCalls)
	require.Len(t, c.decisions, 2)
	for _, decision := range c.decisions {
		var content goalconsent.Content
		require.NoError(t, json.Unmarshal(decision.Content, &content))
		require.NotNil(t, content.Approved)
		assert.False(t, *content.Approved)
	}
}
