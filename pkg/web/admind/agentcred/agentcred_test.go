package agentcred_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

const testSubject = identity.Subject("user:admin@example.org")

func testRequest() agentcred.Request {
	return agentcred.Request{
		SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "tenant-a", Name: "sess-1"},
		Credential: "billing-api-key",
		Token:      "sk_fresh",
	}
}

// TestReplace_SendsTokenAndSubjectAndNothingElse pins the trust boundary on the
// wire: the operator must receive the service token, the asserted subject, and a
// body naming ONLY the session, credential, and value. An identity reference or
// an authorization verdict appearing here would move the decision to the caller.
func TestReplace_SendsTokenAndSubjectAndNothingElse(t *testing.T) {
	var gotAuth, gotSubject string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, agentcred.Path, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotSubject = r.Header.Get(agentcred.SubjectHeader)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		writeJSON(t, w, http.StatusOK, agentcred.Response{
			SecretRef: spiceboxv1alpha1.NamespacedRef{Namespace: "tenant-a", Name: "billing-bot-creds"},
		})
	}))
	t.Cleanup(srv.Close)

	resp, err := agentcred.New(srv.URL, "svc-token").Replace(context.Background(), testSubject, testRequest())
	require.NoError(t, err)
	assert.Equal(t, "billing-bot-creds", resp.SecretRef.Name, "the caller learns the destination it did not choose")

	assert.Equal(t, "Bearer svc-token", gotAuth)
	assert.Equal(t, testSubject.String(), gotSubject, "the proven subject travels as an assertion, in the header")

	keys := make([]string, 0, len(gotBody))
	for k := range gotBody {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"sessionRef", "credential", "token"}, keys,
		"the body must name NO identity, NO secret, and NO authorization verdict")
}

// TestReplace_ErrorMapping — a refusal and a coded failure call for different
// sentences in front of the human, so they must arrive as different errors even
// though both are non-2xx.
func TestReplace_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   agentcred.ErrorBody
		wantIs error
	}{
		{
			name:   "403 with no code: ErrRefused (render the permission page)",
			status: http.StatusForbidden, body: agentcred.ErrorBody{Error: "subject lacks the permission"},
			wantIs: agentcred.ErrRefused,
		},
		{
			name:   "401: ErrRefused too — the caller could not authenticate",
			status: http.StatusUnauthorized, body: agentcred.ErrorBody{Error: "bad token"},
			wantIs: agentcred.ErrRefused,
		},
		{
			name:   "409 value_unchanged: the typed sentinel, so the page says 'paste the NEW one'",
			status: http.StatusConflict,
			body:   agentcred.ErrorBody{Error: "same value", Code: agentidentity.CodeValueUnchanged},
			wantIs: agentidentity.ErrValueUnchanged,
		},
		{
			name:   "409 not_replaceable: the typed sentinel",
			status: http.StatusConflict,
			body:   agentcred.ErrorBody{Error: "oauth", Code: agentidentity.CodeNotReplaceable},
			wantIs: agentidentity.ErrNotReplaceable,
		},
		{
			name:   "403 ambiguous_owner: the coded sentinel wins over the bare refusal",
			status: http.StatusForbidden,
			body:   agentcred.ErrorBody{Error: "two owners", Code: agentidentity.CodeAmbiguousOwner},
			wantIs: agentidentity.ErrAmbiguousOwner,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, tc.status, tc.body)
			}))
			t.Cleanup(srv.Close)

			_, err := agentcred.New(srv.URL, "svc-token").Replace(context.Background(), testSubject, testRequest())
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantIs)
		})
	}
}

// TestReplace_RefusesToSendWhenUnconfigured — a client with no operator URL or
// no service token must fail loudly rather than issue an unauthenticated write
// attempt, and a caller with no proven subject has nothing to assert.
//
// The load-bearing assertion is assert.Zero(hits), NOT require.Error. Pointed at
// an unroutable host (http://operator.invalid — RFC 2606 reserved), these rows
// error on DNS whether or not the guards exist, so the error alone leaves the
// row's actual claim -- "rather than issuing an unauthenticated write attempt"
// -- entirely unverified. A live server that must never be touched is what
// checks it: delete the guards in Replace and every row fires a real POST.
func TestReplace_RefusesToSendWhenUnconfigured(t *testing.T) {
	cases := []struct {
		name string
		// baseURL is computed from the live server's URL so a row can choose
		// between "reachable but must not be reached" and "not configured".
		baseURL func(srvURL string) string
		token   string
		subject identity.Subject
	}{
		{
			name:    "no operator URL: nothing is sent anywhere",
			baseURL: func(string) string { return "" },
			token:   "svc", subject: testSubject,
		},
		{
			name:    "no service token: the reachable operator is NOT called unauthenticated",
			baseURL: func(u string) string { return u },
			token:   "", subject: testSubject,
		},
		{
			name:    "no proven subject: the reachable operator is NOT called with nothing to assert",
			baseURL: func(u string) string { return u },
			token:   "svc", subject: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits++
				// A 200 on purpose: if a guard is ever removed, the row must
				// fail on the write having HAPPENED, not on the response being
				// unhappy.
				writeJSON(t, w, http.StatusOK, agentcred.Response{})
			}))
			t.Cleanup(srv.Close)

			_, err := agentcred.New(tc.baseURL(srv.URL), tc.token).
				Replace(context.Background(), tc.subject, testRequest())

			require.Error(t, err, "an unconfigured client must refuse")
			assert.Zero(t, hits,
				"the operator must never be called by a client that cannot authenticate or cannot say who is asking")
		})
	}
}

// TestReplace_NonJSONErrorBodyStillFails — an unparseable body must never read
// as success; the status and the raw bytes survive into the error.
func TestReplace_NonJSONErrorBodyStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>gateway exploded</html>"))
	}))
	t.Cleanup(srv.Close)

	_, err := agentcred.New(srv.URL, "svc-token").Replace(context.Background(), testSubject, testRequest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
	assert.Contains(t, err.Error(), "gateway exploded")
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(v))
}
