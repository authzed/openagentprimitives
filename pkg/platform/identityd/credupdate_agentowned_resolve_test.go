package identityd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// errAPIServerDown stands in for the apiserver being briefly unable to answer —
// a blip, a rolled control plane, an expired client credential. It says nothing
// whatever about the visitor.
var errAPIServerDown = errors.New("etcdserver: request timed out")

// newResolveFailureFixture builds the standard agent-owned fixture with one
// injected fault: every CredentialUpdateRequest List fails.
//
// That is the single I/O resolveAgentOwnedTarget performs, so it isolates
// "the platform could not look up whose credential this is" from every refusal
// that is genuinely about the visitor. The permission oracle GRANTS the admin,
// which is what makes the assertion below meaningful: any 403 the handler
// produces cannot be blamed on a missing grant.
func newResolveFailureFixture(t *testing.T, authz *fakeAgentAuthz) linkFixture {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agentOwnedObjects()...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl clientpkg.WithWatch, list clientpkg.ObjectList, opts ...clientpkg.ListOption) error {
				if _, ok := list.(*spiceboxv1alpha1.CredentialUpdateRequestList); ok {
					return errAPIServerDown
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()

	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuthenticator{externalBaseURL: "https://identityd.example.org"},
		},
		AgentIdentityAuthz: authz,
	})
	fx := linkFixture{srv: srv, signer: passthroughlink.New(signerKey), c: c}
	fx.web = buildWebHandler(t, fx)
	return fx
}

// TestLinkAgentOwned_ResolveInfrastructureFailureIsNotAPermissionRefusal —
// "I could not look up whose credential this is" and "you are not allowed to
// replace it" are different statements, and rendering the first as the second
// tells a real administrator they lack a permission they actually hold. They
// then go chase a grant that was never missing while the apiserver blip that
// caused it goes unreported.
//
// The refusal page is deliberately uniform for every genuine refusal (it must
// not leak which agent identities exist or who administers them), which is
// exactly why an infrastructure fault must not be routed into it: there is no
// way for the reader to tell the two apart once it is.
//
// Mirrors the branching the operator already applies to the same resolver in
// pkg/web/admind: an ambiguous-owner refusal is a 403, anything else is a 500.
func TestLinkAgentOwned_ResolveInfrastructureFailureIsNotAPermissionRefusal(t *testing.T) {
	cases := []struct {
		name string
		do   func(t *testing.T, fx linkFixture, raw, cookie string) *httptest.ResponseRecorder
	}{
		{
			name: "GET /link: 500, not the not-permitted page",
			do: func(t *testing.T, fx linkFixture, raw, cookie string) *httptest.ResponseRecorder {
				return doGET(t, fx, raw, "", cookie)
			},
		},
		{
			name: "POST /link/submit: 500, not the not-permitted page",
			do: func(t *testing.T, fx linkFixture, raw, cookie string) *httptest.ResponseRecorder {
				form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
				return doPOST(t, fx, form, cookie)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authz := &fakeAgentAuthz{}
			authz.grant(agentOwnedNS, agentOwnedID, adminSubject)
			fx := newResolveFailureFixture(t, authz)

			raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
			cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

			rec := tc.do(t, fx, raw, cookie)

			assert.Equal(t, http.StatusInternalServerError, rec.Code,
				"an apiserver failure is the platform's fault, not the visitor's")
			assert.NotContains(t, rec.Body.String(), "only a platform administrator may do",
				"a transient lookup failure must never tell an admin they lack a permission they hold")
			assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx),
				"nothing may be written when the platform could not even resolve the target")
		})
	}
}

// TestLinkAgentOwned_AmbiguousOwnerIsStillRefused is the other half of the
// branch above, and the reason it is a branch rather than a blanket 500: when
// the platform CAN read the requests and finds they do not name one credential
// owner, refusing is correct and must stay a refusal.
//
// Without this case the fix could regress into "every resolve failure is a
// 500", which would turn a genuine fail-closed refusal into an invitation to
// retry.
func TestLinkAgentOwned_AmbiguousOwnerIsStillRefused(t *testing.T) {
	authz := &fakeAgentAuthz{}
	authz.grant(agentOwnedNS, agentOwnedID, adminSubject)

	// A second request in the same session resolving the SAME credential name to
	// a DIFFERENT AgentIdentity: the platform cannot say which one the pasted
	// value belongs to.
	other := makeCredUpdateRequest(spiceboxv1alpha1.IdentityKindAgentIdentity,
		agentOwnedNS, "other-bot-id", agentOwnedCred,
		&spiceboxv1alpha1.NamespacedRef{Namespace: agentOwnedNS, Name: agentOwnedSecret})
	other.Name = "cur-ambiguous"
	fx := newLinkFixtureWithAuthz(t, authz, authz, agentOwnedObjects(other)...)

	raw := mintCredUpdateLink(t, fx, "", agentOwnedCred)
	cookie := mintCookie(t, fx.signer, adminSubject, time.Time{})

	form := url.Values{"link": {raw}, "credential": {agentOwnedCred}, "token": {agentOwnedFresh}}
	rec := doPOST(t, fx, form, cookie)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an owner the platform cannot pin down is a fail-closed refusal, not a retryable fault")
	assert.Equal(t, agentOwnedStale, agentSecretValue(t, fx))
}
