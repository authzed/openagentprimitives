// pkg/channels/channelkinds/slack/response_url_test.go
//
// The response_url destination guard. postToResponseURL is handed a URL that
// arrives on the WIRE — InteractionAppliedPayload.ResponseRef and
// InteractionDecisionRejectedPayload.ResponseRef are publisher-controlled JSON
// — and channelsd, not the publisher, makes the request. These tests pin that
// the destination is checked before any connection is attempted.
package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// countingServer stands in for whatever the forged URL points at — the
// operator's :8082 debug/memory server, SpiceDB, the API server, the node
// metadata endpoint. The assertion is that it is never contacted at all: a
// guard that let the request out and only ignored the response would still
// have delivered attacker-chosen JSON to an in-cluster service.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestPostToResponseURL_RefusesAnyDestinationThatIsNotSlacks is the guard for
// the confused-deputy / SSRF / egress-bypass finding. channelsd holds a
// network position no session has (a session pinned to
// effectiveNetworkMode: none has no egress of its own), so a publisher that
// chooses the destination is choosing where channelsd's credentials and
// network position are spent.
func TestPostToResponseURL_RefusesAnyDestinationThatIsNotSlacks(t *testing.T) {
	srv, hits := countingServer(t)

	cases := []struct {
		name string
		url  func() string
		// wantErr is the substring naming WHICH property of the destination was
		// wrong, so an operator reading the log learns that rather than
		// "request failed". Scheme is checked before host, so a plaintext URL
		// is refused on its scheme whatever host it names.
		wantErr string
	}{
		{
			name:    "an in-cluster service over https: refused on its host, not contacted",
			url:     func() string { return "https://ap-operator.agentprimitives.svc.cluster.local:8082/memory/put" },
			wantErr: "host",
		},
		{
			name:    "the node metadata endpoint: refused, not contacted",
			url:     func() string { return "https://169.254.169.254/computeMetadata/v1/instance/service-accounts/" },
			wantErr: "host",
		},
		{
			name:    "a lookalike host that merely PREFIXES Slack's: refused on its host",
			url:     func() string { return "https://hooks.slack.com.attacker.example.invalid/actions/x" },
			wantErr: "host",
		},
		{
			name:    "Slack's own host over plaintext http: refused (the body is a decision record)",
			url:     func() string { return "http://hooks.slack.com/actions/T0/1/abc" },
			wantErr: "scheme",
		},
		{
			name:    "an in-cluster service over plaintext http: refused on its scheme, not contacted",
			url:     func() string { return "http://ap-operator.agentprimitives.svc.cluster.local:8082/memory/put" },
			wantErr: "scheme",
		},
		{
			name:    "a live httptest server, reachable and willing: still refused",
			url:     func() string { return srv.URL + "/actions/x" },
			wantErr: "scheme", // httptest.NewServer is plaintext; the host is wrong too
		},
		{
			name:    "an unparseable URL: refused",
			url:     func() string { return "://not a url" },
			wantErr: "invalid response_url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := postToResponseURL(context.Background(), srv.Client(), tc.url(), map[string]any{"text": "x"})
			require.Error(t, err, "a non-Slack response_url must be refused")
			assert.Contains(t, err.Error(), tc.wantErr, "the refusal must say what was wrong with the destination")
			assert.Contains(t, err.Error(), responseURLHost,
				"...and name the one destination that IS allowed, so the log explains itself")
		})
	}
	assert.Zero(t, hits.Load(), "the guard must refuse BEFORE connecting — nothing may reach the destination")
}

// TestPostToResponseURL_AcceptsSlacksOwnResponseURL is the false-positive
// guard: the real thing must still go through, or every approval click stops
// editing the clicker's message.
func TestPostToResponseURL_AcceptsSlacksOwnResponseURL(t *testing.T) {
	bodies, srv := newResponseURLServer(t)
	t.Cleanup(srv.Close)

	require.NoError(t, postToResponseURL(context.Background(), responseURLClient(srv), testResponseURL,
		map[string]any{"replace_original": true, "text": "approved"}), "a genuine Slack response_url must post")
	got := bodies.values()
	require.Len(t, got, 1, "want one POST")
	assert.Contains(t, got[0], `"replace_original":true`)
}

// TestInteractionSender_ForgedResponseRef_RefusedAndLogged drives the defect
// end to end through the two senders that read a WIRE ResponseRef. A refusal
// that were silent would be worse than the bug it fixes: the surface simply
// would not update and nobody would know why. Both paths must leave a log line
// an operator can grep, and the applied path must still complete the rest of
// its resolution bookkeeping.
func TestInteractionSender_ForgedResponseRef_RefusedAndLogged(t *testing.T) {
	const forged = "http://ap-operator.agentprimitives.svc.cluster.local:8082/memory/put"

	cases := []struct {
		name  string
		env   func(t *testing.T) channelevents.Envelope
		check func(t *testing.T, fc *fakeSlackClient, err error)
	}{
		{
			name: "interaction_applied: refused, logged, and the other surfaces still edited",
			env: func(t *testing.T) channelevents.Envelope {
				t.Helper()
				return interactionEnvelope(t, channelevents.KindInteractionApplied,
					channelevents.InteractionAppliedPayload{
						Category: "permission_request", RequestRef: "r1",
						Outcome: channelevents.OutcomeApproved, OutcomeText: "approved",
						ResponseRef: forged,
					})
			},
			check: func(t *testing.T, fc *fakeSlackClient, err error) {
				require.NoError(t, err, "the applied edit is best-effort; a refused response_url must not abort resolution")
				assert.Contains(t, fc.updatedRefs, "DOWNER:1.1", "the recorded prompt is still edited")
				assert.Contains(t, fc.updatedRefs, "CHAN:2.2", "the public note is still edited")
			},
		},
		{
			name: "interaction_decision_rejected: refused and surfaced as an error the relay logs",
			env: func(t *testing.T) channelevents.Envelope {
				t.Helper()
				return interactionEnvelope(t, channelevents.KindInteractionDecisionRejected,
					channelevents.InteractionDecisionRejectedPayload{
						RequestRef: "r1", Category: "permission_request",
						Class: "no_standing", Reason: "you are not the approver",
						ResponseRef: forged,
					})
			},
			check: func(t *testing.T, _ *fakeSlackClient, err error) {
				require.Error(t, err, "the rejection path has no other surface, so the refusal is returned")
				assert.Contains(t, err.Error(), "refusing response_url", "the returned error names the refusal")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged strings.Builder
			ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) {
				logged.WriteString(args)
				logged.WriteString("\n")
			}, funcr.Options{}))

			fc := &fakeSlackClient{}
			// A client that would happily reach anything, so the refusal is
			// provably the guard's and not the transport's.
			s := &interactionSender{client: fc, httpClient: http.DefaultClient, delivery: newInteractionDeliveryStore()}
			s.delivery.record("r1", deliveryRef{ChannelID: "DOWNER", TS: "1.1"}, deliveryRef{ChannelID: "CHAN", TS: "2.2"})

			_, err := s.Send(ctx, channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, tc.env(t))
			tc.check(t, fc, err)

			// Whichever route it took, an operator must be able to find it.
			combined := logged.String()
			if err != nil {
				combined += err.Error()
			}
			assert.Contains(t, combined, "hooks.slack.com",
				"the refusal must name the only destination that IS allowed, so the log explains itself")
		})
	}
}
