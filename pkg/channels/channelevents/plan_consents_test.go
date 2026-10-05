package channelevents

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func planConsentFixture() InteractionRequestPayload {
	expires := time.Now().Add(time.Hour)
	owner := ExternalIdentity{Kind: "email", ExternalID: "alice@example.com", Email: "alice@example.com"}
	ref := SessionRef{Namespace: "team", Name: "session"}
	child := InteractionRequestPayload{AgentSessionRef: ref, Category: "goal_execution_consent", RequestRef: "one", Lead: "Allow one private reminder?", Body: "A fresh action plan will be required.", Fields: []InteractionField{{Label: "Run once", Value: "2030-01-01T12:00:00Z"}, {Label: "Private recipient", Value: "alice@example.com"}}, Excerpt: &InteractionExcerpt{Label: "Goal", Content: "Stand up and stretch"}, Audience: InteractionAudience{Scope: AudienceRequester, Requester: &owner}, ExpiresAt: &expires, Actions: []InteractionAction{{ID: "approve", Label: "Approve", Kind: ActionKindDecision}}}
	parent := InteractionRequestPayload{AgentSessionRef: ref, Category: "plan_phase", RequestRef: "plan", Lead: "Approve plan and reminder", Consents: []InteractionRequestPayload{child}, Audience: InteractionAudience{Scope: AudienceRequester, Requester: &owner}, ExpiresAt: &expires, Actions: child.Actions}
	parent.Details, _ = json.Marshal(parent.Consents)
	parent.Fields, parent.Excerpt = PlanConsentFields(parent.Consents), PlanConsentExcerpt(parent.Consents)
	return parent
}

func TestPlanConsentWireRejectsHiddenAndRewrittenAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*InteractionRequestPayload)
	}{
		{"complete reviewed card succeeds", func(*InteractionRequestPayload) {}},
		{"hidden fields reject", func(p *InteractionRequestPayload) { p.Fields = nil }},
		{"rewritten time rejects", func(p *InteractionRequestPayload) { p.Consents[0].Fields[0].Value = "2030-01-01T13:00:00Z" }},
		{"hidden evidence rejects", func(p *InteractionRequestPayload) { p.Excerpt = nil }},
		{"nested consent rejects", func(p *InteractionRequestPayload) {
			p.Consents[0].Consents = []InteractionRequestPayload{p.Consents[0]}
		}},
		{"foreign session rejects", func(p *InteractionRequestPayload) { p.Consents[0].AgentSessionRef.Name = "foreign" }},
		{"duplicate consent rejects", func(p *InteractionRequestPayload) {
			p.Consents = append(p.Consents, p.Consents[0])
			p.Fields = PlanConsentFields(p.Consents)
			p.Excerpt = PlanConsentExcerpt(p.Consents)
			p.Details, _ = json.Marshal(p.Consents)
		}},
		{"parent outliving request rejects", func(p *InteractionRequestPayload) { later := p.ExpiresAt.Add(time.Hour); p.ExpiresAt = &later }},
		{"unreviewably large exact details reject", func(p *InteractionRequestPayload) {
			p.Consents[0].Body = strings.Repeat("x", 128*1024)
			p.Fields = PlanConsentFields(p.Consents)
			p.Details, _ = json.Marshal(p.Consents)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planConsentFixture()
			tc.mutate(&p)
			if tc.name == "complete reviewed card succeeds" {
				require.NoError(t, p.Validate())
				raw, err := json.Marshal(p)
				require.NoError(t, err)
				var replay InteractionRequestPayload
				require.NoError(t, json.Unmarshal(raw, &replay))
				assert.Equal(t, p.Fields, replay.Fields)
				assert.Equal(t, p.Excerpt, replay.Excerpt)
				require.NoError(t, replay.Validate())
			} else {
				assert.Error(t, p.Validate())
			}
		})
	}
}
