package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// heartbeatFixture extends the shared linkFixture shape with a K8s client
// whose fake includes WithStatusSubresource for SessionUserIdentity.
// That wiring is required for s.deps.K8s.Status().Update() to behave as it
// would against a real API server (status and spec are separate sub-resources).
type heartbeatFixture struct {
	srv    *Server
	signer *passthroughlink.Signer
	c      clientpkg.Client
}

// newHeartbeatFixture builds a fixture pre-seeded with the given objects.
// The fake client is configured with WithStatusSubresource so Status().Update
// writes are accepted by the fake rather than being silently rejected.
func newHeartbeatFixture(t *testing.T, objs ...clientpkg.Object) heartbeatFixture {
	t.Helper()
	scheme := newScheme(t) // already registers corev1 + spiceboxv1alpha1
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.SessionUserIdentity{}).
		Build()
	signer := passthroughlink.New(signerKey)
	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
	})
	return heartbeatFixture{srv: srv, signer: signer, c: c}
}

// makeSUI returns a SessionUserIdentity in the same namespace+name as its
// session (the conventional 1:1 mapping the heartbeat handler relies on).
func makeSUI(ns, name, subject string) *spiceboxv1alpha1.SessionUserIdentity {
	return &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: name,
			UserIdentity: "ui-" + name,
			Subject:      subject,
		},
	}
}

// doHeartbeat sends a POST /heartbeat?session=<sessionRef> with an optional
// cookie. Returns the recorder.
func doHeartbeat(t *testing.T, srv *Server, sessionRef, cookieValue string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/heartbeat"
	if sessionRef != "" {
		target += "?session=" + sessionRef
	}
	req := httptest.NewRequest(http.MethodPost, target, nil)
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// === POST /heartbeat tests ===================================================

func TestHandleHeartbeat(t *testing.T) {
	const (
		ns    = "default"
		name  = "sess-hb"
		alice = "user:alice@example.com"
		bob   = "user:bob@example.com"
	)

	// Pre-build common test objects shared across cases via closures.
	sessByAlice := makeAgentSession(ns, name, alice)
	suiAlice := makeSUI(ns, name, alice)

	cases := []struct {
		name string

		// objects is the set of K8s objects seeded in the fake client.
		objects []clientpkg.Object

		// cookieSubject is the subject encoded in the idd_session cookie.
		// "" means no cookie is sent.
		cookieSubject identity.Subject

		// sessionRef is the ?session= query value. "" means the param is
		// absent (tests the missing/malformed branch).
		sessionRef string

		// method overrides the HTTP method (default POST).
		method string

		wantStatus  int
		wantBodyHas string

		// checkSUIUpdated, when true, asserts that the SUI's
		// LastInteractionAt was set to a recent timestamp.
		checkSUIUpdated bool
	}{
		{
			name:            "happy path: valid cookie + matching session + existing SUI → 204 + LastInteractionAt updated",
			objects:         []clientpkg.Object{sessByAlice, suiAlice},
			cookieSubject:   alice,
			sessionRef:      ns + "/" + name,
			wantStatus:      http.StatusNoContent,
			checkSUIUpdated: true,
		},
		{
			name:          "no cookie → 403",
			objects:       []clientpkg.Object{sessByAlice, suiAlice},
			cookieSubject: "",
			sessionRef:    ns + "/" + name,
			wantStatus:    http.StatusForbidden,
			wantBodyHas:   "no session cookie",
		},
		{
			name:          "malformed session ref: empty → 400",
			objects:       []clientpkg.Object{},
			cookieSubject: alice,
			sessionRef:    "",
			wantStatus:    http.StatusBadRequest,
			wantBodyHas:   "session=<ns>/<name> required",
		},
		{
			name:          "malformed session ref: missing namespace → 400",
			objects:       []clientpkg.Object{},
			cookieSubject: alice,
			sessionRef:    "/noname",
			wantStatus:    http.StatusBadRequest,
			wantBodyHas:   "session=<ns>/<name> required",
		},
		{
			name:          "malformed session ref: missing name → 400",
			objects:       []clientpkg.Object{},
			cookieSubject: alice,
			sessionRef:    "default/",
			wantStatus:    http.StatusBadRequest,
			wantBodyHas:   "session=<ns>/<name> required",
		},
		{
			name:          "session not found: valid cookie, no AgentSession → 404",
			objects:       []clientpkg.Object{},
			cookieSubject: alice,
			sessionRef:    ns + "/" + name,
			wantStatus:    http.StatusNotFound,
			wantBodyHas:   "session not found",
		},
		{
			name: "subject mismatch: cookie for bob, session started by alice → 403, SUI untouched",
			objects: []clientpkg.Object{
				makeAgentSession(ns, name, alice), // session belongs to alice
				makeSUI(ns, name, alice),
			},
			cookieSubject: bob, // bob's cookie
			sessionRef:    ns + "/" + name,
			wantStatus:    http.StatusForbidden,
			wantBodyHas:   "session belongs to a different user",
		},
		{
			name: "SUI not found: session exists but SUI not created yet → 404, no side effects",
			objects: []clientpkg.Object{
				makeAgentSession(ns, name, alice), // session present, SUI absent
			},
			cookieSubject: alice,
			sessionRef:    ns + "/" + name,
			wantStatus:    http.StatusNotFound,
			wantBodyHas:   "no session-user-identity",
		},
		{
			name:          "wrong method GET → 405",
			objects:       []clientpkg.Object{},
			cookieSubject: alice,
			sessionRef:    ns + "/" + name,
			method:        http.MethodGet,
			wantStatus:    http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHeartbeatFixture(t, tc.objects...)

			var cookieVal string
			if tc.cookieSubject != "" {
				cookieVal = mintCookie(t, fx.signer, tc.cookieSubject, time.Time{})
			}

			// Build request; allow method override for the 405 case.
			method := http.MethodPost
			if tc.method != "" {
				method = tc.method
			}
			target := "/heartbeat"
			if tc.sessionRef != "" {
				target += "?session=" + tc.sessionRef
			}
			req := httptest.NewRequest(method, target, nil)
			if cookieVal != "" {
				req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieVal})
			}
			rec := httptest.NewRecorder()
			fx.srv.Handler().ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code, "status code mismatch")
			if tc.wantBodyHas != "" {
				assert.Contains(t, rec.Body.String(), tc.wantBodyHas, "response body mismatch")
			}

			if tc.checkSUIUpdated {
				var got spiceboxv1alpha1.SessionUserIdentity
				require.NoError(t, fx.c.Get(context.Background(),
					clientpkg.ObjectKey{Namespace: ns, Name: name}, &got),
					"re-Get SUI after heartbeat")
				require.NotNil(t, got.Status.LastInteractionAt,
					"LastInteractionAt must be set after a successful heartbeat")
				age := time.Since(got.Status.LastInteractionAt.Time)
				assert.Less(t, age, 5*time.Second,
					"LastInteractionAt must be recent (within 5s), got age=%v", age)
			}

			// For error cases that have a SUI in the store (subject-mismatch
			// case), verify the SUI was NOT modified.
			if !tc.checkSUIUpdated {
				var got spiceboxv1alpha1.SessionUserIdentity
				err := fx.c.Get(context.Background(),
					clientpkg.ObjectKey{Namespace: ns, Name: name}, &got)
				if err == nil {
					// SUI found — its LastInteractionAt must remain nil (untouched).
					assert.Nil(t, got.Status.LastInteractionAt,
						"LastInteractionAt must NOT be set on error paths")
				}
				// If SUI not found, there is nothing to assert about its state.
			}
		})
	}
}

// TestHandleHeartbeat_NoContent_NoBody verifies that a successful heartbeat
// returns exactly 204 with an empty body — the browser JS never parses the
// response but log scraping + metric tools may check.
func TestHandleHeartbeat_NoContent_NoBody(t *testing.T) {
	const (
		ns    = "default"
		name  = "sess-nc"
		alice = "user:alice@example.com"
	)
	fx := newHeartbeatFixture(t,
		makeAgentSession(ns, name, alice),
		makeSUI(ns, name, alice),
	)
	cookie := mintCookie(t, fx.signer, alice, time.Time{})
	rec := doHeartbeat(t, fx.srv, ns+"/"+name, cookie)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "204 must have an empty body")

	// Verify the scheme / content-type header: http.Error sets text/plain;
	// our handler MUST NOT call http.Error on the success path.
	assert.Empty(t, rec.Header().Get("Content-Type"),
		"success path must not set Content-Type (no body)")
}

// Ensure the newScheme helper registers corev1 so the fake client can
// store Secret objects (exercised indirectly via useridentity helpers in
// link tests; confirmed here as a compile-time check via usage of corev1).
var _ = corev1.Pod{}
