package admind_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// fakeIdentityReader is a scripted admind.SubjectIdentityReader that records
// the canonical id it was actually called with — the point being to catch
// admind.New's own subject → CanonicalUserID conversion getting it wrong, a
// failure the section-builder unit tests in
// pkg/web/admind/config/projectors/detail_test.go cannot see: they drive
// directoryIdentitiesSection directly with an explicit slice/error, never
// through admind.New's wrapper.
type fakeIdentityReader struct {
	calls        int
	gotCanonical string
	ids          spicedb.SubjectIdentities
	err          error
}

func (f *fakeIdentityReader) ListSubjectIdentities(_ context.Context, canonicalID identity.CanonicalUserID) (spicedb.SubjectIdentities, error) {
	f.calls++
	f.gotCanonical = canonicalID.String()
	return f.ids, f.err
}

// TestAdmindIdentities_ReaderReceivesBareCanonicalNotPrefixedSubject proves
// admind.New strips UserIdentity.Spec.Subject's "user:" prefix before calling
// ListSubjectIdentities, rather than passing the SpiceDB subject reference
// through whole. Getting this backwards (i.e. calling
// identity.CanonicalFromTrusted directly on the raw "user:<canonical>"
// subject) makes ListSubjectIdentities filter on "user:user:<canonical>" —
// never matching a real SpiceDB object id — so the Directory identities panel
// would render empty for every user, forever, with every other test in this
// task still green. That silent-wrong-answer is exactly why this guard
// exists: it is the one line none of the six other tests added for this
// feature exercises.
func TestAdmindIdentities_ReaderReceivesBareCanonicalNotPrefixedSubject(t *testing.T) {
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "hashabc"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:YWxpY2U="},
	}
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(ui).Build()
	reader := &fakeIdentityReader{}

	a, err := admind.New(admind.Config{
		Mem:        memory.NewLocal(inmem.NewBackend()),
		K8s:        k8s,
		Checker:    stubChecker{allow: map[string]bool{"YWRtaW4": true}},
		Token:      "test-token",
		Logger:     testr.New(t),
		Identities: reader,
	})
	require.NoError(t, err)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/users/user:YWxpY2U=", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	require.Equal(t, 1, reader.calls, "the reader must be called exactly once for this detail read")
	assert.Equal(t, "YWxpY2U=", reader.gotCanonical,
		`the reader must receive the BARE canonical id ("YWxpY2U="), not the "user:"-prefixed subject reference`)
}

// A partial read must reach the page as BOTH halves: the links that were read,
// and a statement that the list is short.
//
// This is the whole-path guard for the all-or-nothing read the errgroup used to
// do. github_org / github_team / github_repo are declared only in the gh toolkit
// fragment, composed into the live schema by the guardian reconciler, so before
// that composition a probe against them errors — and one error used to abort
// every probe, blanking the panel for EVERY user with "unavailable:". Rendering
// only the rows would be the opposite failure: a short list presented as a
// complete one.
func TestAdmindIdentities_PartialReadKeepsRowsAndSaysWhatIsMissing(t *testing.T) {
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "hashabc"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:YWxpY2U="},
	}
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(ui).Build()
	reader := &fakeIdentityReader{ids: spicedb.SubjectIdentities{
		Identities: []spicedb.SubjectIdentity{
			{Definition: "slack_user", Relation: "user", ObjectID: "U0FKE", Source: "Slack"},
		},
		Unavailable: []spicedb.UnavailableProbe{
			{Source: "GitHub", Definition: "github_org", Relation: "member", Err: "object definition `github_org` not found"},
		},
	}}

	a, err := admind.New(admind.Config{
		Mem:        memory.NewLocal(inmem.NewBackend()),
		K8s:        k8s,
		Checker:    stubChecker{allow: map[string]bool{"YWRtaW4": true}},
		Token:      "test-token",
		Logger:     testr.New(t),
		Identities: reader,
	})
	require.NoError(t, err)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/config/users/user:YWxpY2U=", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	assert.Contains(t, body, "slack_user:U0FKE",
		"the link that WAS read must still be rendered — one absent definition cannot blank the panel")
	assert.Contains(t, body, "INCOMPLETE",
		"the page must say the list is short rather than present it as finished")
	assert.Contains(t, body, "github_org",
		"and it must name WHICH source could not be read, so the condition is actionable")
}
