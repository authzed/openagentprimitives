// pkg/controllers/relationshipsource/monitoring_scrub_test.go
//
// A MonitoringEvent.Summary is the furthest an error string travels: straight
// out to every role=monitoring Channel, typically a Slack channel, without
// passing through a condition on the way. These are the two Summaries this
// controller composes itself, and they were the half of the scrubbing story
// that was missing — status was protected and chat was not.
package relationshipsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// The concrete leak, end to end through a real Reconcile. credhost.Check
// quotes the FULL raw destination when spec.baseURL names no host it can match
// — so a baseURL carrying a token in its query string rode into
// publishCredResolveFailed's Summary and out to every monitoring Channel,
// while the identical text was already being scrubbed on its way into the
// Ready condition on the very same reconcile.
func TestMonitoring_CredentialResolutionSummaryCarriesNoCredential(t *testing.T) {
	const kindName = "fakekind-credresolve-scrub"
	const token = "notarealtoken-abc123"

	src := newSrc("ns", "src", kindName)
	// Parses, but names no host, so credhost.Check takes the branch that
	// quotes rawURL in full rather than the one that quotes only the host.
	src.Spec.BaseURL = "/Groups?access_token=" + token
	id, sec := authFixtures("ns", "directory.example.internal")
	c := newClient(t, src, id, sec)

	relsync.Register(&fakeKind{name: kindName, source: relsource.Source{Name: kindName + "-sync"}})
	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish}

	key := types.NamespacedName{Namespace: "ns", Name: "src"}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err, "an auth refusal is condition-surfaced, never returned")

	events := pub.snapshot()
	require.Len(t, events, 1, "precondition: the refusal is reported to the monitoring bus")
	require.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed, events[0].Reason)
	assert.NotContains(t, events[0].Summary, token,
		"a Summary goes straight to a chat channel; a credential must not ride there")
	assert.Contains(t, events[0].Summary, "credential resolution failed",
		"scrubbing must not cost the diagnosis")

	// The condition was already safe. Asserted anyway, because "both ends are
	// scrubbed" is the claim, and a fix that moved the leak rather than closing
	// it would satisfy only one of these.
	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &got))
	ready := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	assert.NotContains(t, ready.Message, token)
}

// The other self-composed Summary. Today's emitted buckets happen to hold only
// errors this codebase generates itself, so this is a guard against the
// accident changing rather than a live leak — classification is by SENTINEL,
// not by origin, so a kind error wrapping relsource.ErrRefused lands in an
// emitted bucket quoting whatever upstream said.
func TestMonitoring_ScopeFailedSummaryCarriesNoCredential(t *testing.T) {
	const kindName = "fakekind-scopesummary-scrub"
	const token = "notarealtoken-def456"

	src := newSrc("ns", "src", kindName)
	id, sec := authFixtures("ns")
	c := newClient(t, src, id, sec)

	fk := &fakeKind{
		name:   kindName,
		source: relsource.Source{Name: kindName + "-sync"},
		pages:  []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
		fetchErrs: map[relsync.ScopeID]error{
			"C1": fmt.Errorf("upstream: GET https://directory.example.internal/Groups?access_token=%s: %w",
				token, relsource.ErrRefused),
		},
	}
	relsync.Register(fk)

	sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
	pub := &publishCapture{}
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "src"},
	})
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 1, "precondition: a guard refusal is an emitted issue")
	assert.NotContains(t, events[0].Summary, token)
	assert.Contains(t, events[0].Summary, "C1", "the scope must still be named, or the alert is not actionable")
}

// Every Summary this controller composes is also BOUNDED, because
// scrubScopeErrorMessage bounds what it returns. An upstream answering with a
// page of HTML must not be able to put that page into a chat channel.
func TestMonitoring_SelfComposedSummariesAreBounded(t *testing.T) {
	huge := strings.Repeat("x", 8000)

	scoped := scopeFailedSummary(issueGuardRefusal, []relsync.ScopeID{"C1"},
		map[relsync.ScopeID]relsync.ScopeError{"C1": {Scope: "C1", Err: errors.New(huge)}})
	assert.Less(t, len([]rune(scoped)), 1000,
		"a scope-failure Summary must be bounded, not however long upstream's body was")

	// The credential Summary's own bound comes from the same call; asserted
	// here rather than through a Reconcile because the point is the bound, not
	// the path.
	assert.LessOrEqual(t, len([]rune(scrubScopeErrorMessage(huge))), maxScopeErrorMessageLen)
}
