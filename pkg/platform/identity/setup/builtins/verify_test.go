package builtins

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// verifyProv builds a bearer provider pointing its verify: probe at url.
func verifyProv(t *testing.T, url string) *provider.Provider {
	t.Helper()
	return &provider.Provider{
		ID:    "test-prov",
		Title: "TestProv",
		Shape: "bearer",
		Verify: &provider.VerifyConfig{
			Endpoint:     url,
			AuthScheme:   "token",
			SubjectField: "login",
		},
	}
}

// installVerifyClient points VerifyHTTPBearer at a plain http.Client for
// the test's lifetime (safehttp blocks loopback, which httptest uses).
func installVerifyClient(t *testing.T) {
	t.Helper()
	SetVerifyHTTPClient(func() *http.Client { return &http.Client{} })
	t.Cleanup(func() { SetVerifyHTTPClient(nil) })
}

func TestVerifyHTTPBearer(t *testing.T) {
	installVerifyClient(t)
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		wantStatus  VerifyStatus
		wantSubject string
		wantDetail  string // substring
	}{
		{
			name: "200 with subject field: Valid, subject extracted, auth header sent",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "token tok-123" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"login":"octocat"}`))
			},
			wantStatus:  VerifyValid,
			wantSubject: "octocat",
			wantDetail:  "octocat",
		},
		{
			name: "401 bad credentials: Rejected with body message",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			},
			wantStatus: VerifyRejected,
			wantDetail: "Bad credentials",
		},
		{
			// A live token that merely lacks access answers 403 exactly as a
			// revoked one does, so 403 can never be the definitive rejection
			// that opens a credential-update card unaided. It is its own
			// verdict rather than Indeterminate because consumers must be able
			// to tell it apart from a network timeout: one is worth telling a
			// human standing at a form about, the other is not.
			name: "403 plain: Forbidden — the credential authenticated, this one check did not pass",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantStatus: VerifyForbidden,
			wantDetail: "accepted the credential but refused this check",
		},
		{
			name: "403 with an authorization message: Forbidden, the provider's reason is carried through",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"Resource protected by organization SAML enforcement."}`))
			},
			wantStatus: VerifyForbidden,
			wantDetail: "SAML enforcement",
		},
		{
			// The throttle hoist runs before the status switch, so a 403 that is
			// really a rate limit stays Indeterminate rather than becoming
			// Forbidden: nothing about it says the credential was even looked at.
			name: "403 rate limit: Indeterminate, never rejects or blames a throttled-but-valid token",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for ..."}`))
			},
			wantStatus: VerifyIndeterminate,
			wantDetail: "rate limit",
		},
		{
			// The throttle check reads the body, not the status: a provider
			// that answers 429 rather than 403 gets the same honest wording.
			name: "429 rate limit: Indeterminate, reported as a throttle rather than an unexpected status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for ..."}`))
			},
			wantStatus: VerifyIndeterminate,
			wantDetail: "rate limiting verification",
		},
		{
			name: "500: Indeterminate",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantStatus: VerifyIndeterminate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			res := VerifyHTTPBearer(context.Background(), verifyProv(t, srv.URL), "tok-123")
			assert.Equal(t, tc.wantStatus, res.Status)
			assert.Equal(t, tc.wantSubject, res.Subject)
			if tc.wantDetail != "" {
				assert.Contains(t, res.Detail, tc.wantDetail)
			}
		})
	}
}

// The stable id and the human-readable subject are DIFFERENT fields on the same
// probe response, extracted independently. GitHub's `login` is what a person
// should read; it is also mutable and reclaimable, so the graph keys on `id`.
//
// `id` arrives as a JSON number. encoding/json decodes an untyped number into
// float64, so a plain string type-assertion silently drops it — which is exactly
// what the pre-existing SubjectField extraction does.
func TestVerifyHTTPBearer_ExtractsAStableSubjectIDAlongsideTheSubject(t *testing.T) {
	installVerifyClient(t)
	cases := []struct {
		name          string
		body          string
		subjectIDF    string
		wantSubject   string
		wantSubjectID string
	}{
		{
			name:          "github shape: login for humans, numeric id for the graph",
			body:          `{"login":"octocat","id":583231,"node_id":"MDQ6VXNlcjU4MzIzMQ=="}`,
			subjectIDF:    "id",
			wantSubject:   "octocat",
			wantSubjectID: "583231",
		},
		{
			name:          "a string id field still works",
			body:          `{"login":"octocat","id":"583231"}`,
			subjectIDF:    "id",
			wantSubject:   "octocat",
			wantSubjectID: "583231",
		},
		{
			name:          "no subjectIDField declared: SubjectID stays empty, Subject unaffected",
			body:          `{"login":"octocat","id":583231}`,
			subjectIDF:    "",
			wantSubject:   "octocat",
			wantSubjectID: "",
		},
		{
			name:          "large id does not go scientific and grows no decimal point",
			body:          `{"login":"octocat","id":123456789012345}`,
			subjectIDF:    "id",
			wantSubject:   "octocat",
			wantSubjectID: "123456789012345",
		},
		{
			name:          "a fractional value is refused rather than truncated",
			body:          `{"login":"octocat","id":1.5}`,
			subjectIDF:    "id",
			wantSubject:   "octocat",
			wantSubjectID: "",
		},
		{
			name:          "BOUNDARY: 2^53+1 exceeds float64 precision but preserves its literal string; json.Number carries the original",
			body:          `{"login":"octocat","id":9007199254740993}`,
			subjectIDF:    "id",
			wantSubject:   "octocat",
			wantSubjectID: "9007199254740993",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			prov := &provider.Provider{
				ID: "probe",
				Verify: &provider.VerifyConfig{
					Endpoint:       srv.URL,
					SubjectField:   "login",
					SubjectIDField: tc.subjectIDF,
				},
			}
			res := VerifyHTTPBearer(context.Background(), prov, "tok")

			require.Equal(t, VerifyValid, res.Status, "detail: %s", res.Detail)
			assert.Equal(t, tc.wantSubject, res.Subject, "the human-readable subject is unchanged")
			assert.Equal(t, tc.wantSubjectID, res.SubjectID, "the stable id keys the graph")
		})
	}
}

func TestVerifyHTTPBearer_NetworkErrorIsIndeterminate(t *testing.T) {
	installVerifyClient(t)
	// Closed server → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	res := VerifyHTTPBearer(context.Background(), verifyProv(t, url), "tok-123")
	assert.Equal(t, VerifyIndeterminate, res.Status)
	assert.NotEmpty(t, res.Detail, "no-silent-errors: detail must carry the underlying error")
}

func TestVerifyHTTPBearer_TimeoutIsIndeterminate(t *testing.T) {
	installVerifyClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res := VerifyHTTPBearer(ctx, verifyProv(t, srv.URL), "tok-123")
	assert.Equal(t, VerifyIndeterminate, res.Status)
}

// TestForbiddenNotice pins the copy every surface shows for this verdict.
// Telling somebody their credential failed verification when it authenticated
// fine is what sent people off to replace credentials that worked, so the
// sentence must state both halves — it authenticated, and this one check was
// refused — and must never read as a rejection or an expiry.
func TestForbiddenNotice(t *testing.T) {
	t.Run("carries the provider's own reason and never reads as a rejection", func(t *testing.T) {
		notice := ForbiddenNotice(VerifyResult{
			Status: VerifyForbidden,
			Detail: "TestProv accepted the credential but refused this check — Resource protected by organization SAML enforcement.",
		})
		assert.Contains(t, notice, "authenticated", "the notice must say the credential DID authenticate")
		assert.Contains(t, notice, "refused for this check", "…and that it was this check, not the credential, that was refused")
		assert.Contains(t, notice, "SAML enforcement", "the provider's own reason must survive into the notice")
		for _, wrong := range []string{"verification failed", "rejected", "expired", "invalid", "403"} {
			assert.NotContains(t, strings.ToLower(notice), wrong,
				"a refused check is not a %s; that wording is what makes people replace a working credential", wrong)
		}
	})

	t.Run("states the framing on its own when the flow supplied no detail", func(t *testing.T) {
		notice := ForbiddenNotice(VerifyResult{Status: VerifyForbidden})
		assert.Contains(t, notice, "authenticated")
		assert.Contains(t, notice, "refused for this check")
	})
}

func TestVerifyHTTPBearer_NoConfigIsUnsupported(t *testing.T) {
	res := VerifyHTTPBearer(context.Background(), &provider.Provider{ID: "p", Shape: "bearer"}, "tok")
	assert.Equal(t, VerifyUnsupported, res.Status)
	res = VerifyHTTPBearer(context.Background(), nil, "tok")
	require.Equal(t, VerifyUnsupported, res.Status)
}

// scriptedFlow is a registry fake whose Verify returns a fixed result.
type scriptedFlow struct {
	name   string
	result VerifyResult
	err    error
	gotReq *VerifyRequest
}

func (f *scriptedFlow) Name() string { return f.name }
func (f *scriptedFlow) Screens(context.Context, Request) ([]tui.Screen, error) {
	return nil, errors.New("scriptedFlow asks nothing; it exists for its Verify")
}
func (f *scriptedFlow) Result(context.Context, Request, *tui.State) error {
	return errors.New("scriptedFlow stores nothing; it exists for its Verify")
}
func (f *scriptedFlow) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	f.gotReq = &req
	return f.result, f.err
}

func TestVerifyCredential(t *testing.T) {
	installVerifyClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	cases := []struct {
		name       string
		prov       *provider.Provider
		flow       *scriptedFlow
		value      StoreValue
		wantStatus VerifyStatus
	}{
		{
			name:       "nil provider: Unsupported",
			wantStatus: VerifyUnsupported,
		},
		{
			name:       "registered builtin: flow.Verify result wins",
			prov:       &provider.Provider{ID: "p1", Builtin: "flow-1"},
			flow:       &scriptedFlow{name: "flow-1", result: VerifyResult{Status: VerifyValid, Subject: "me"}},
			value:      StoreValue{Bearer: "tok"},
			wantStatus: VerifyValid,
		},
		{
			name:       "flow.Verify errors: mapped to Indeterminate, never Rejected",
			prov:       &provider.Provider{ID: "p2", Builtin: "flow-2"},
			flow:       &scriptedFlow{name: "flow-2", err: fmt.Errorf("boom")},
			value:      StoreValue{Bearer: "tok"},
			wantStatus: VerifyIndeterminate,
		},
		{
			name:       "no builtin, catalog verify block, bearer value: declarative probe runs (401 → Rejected)",
			prov:       &provider.Provider{ID: "p3", Verify: &provider.VerifyConfig{Endpoint: "PLACEHOLDER"}},
			value:      StoreValue{Bearer: "tok"},
			wantStatus: VerifyRejected,
		},
		{
			name:       "no builtin, no verify block: Unsupported",
			prov:       &provider.Provider{ID: "p4"},
			value:      StoreValue{Bearer: "tok"},
			wantStatus: VerifyUnsupported,
		},
		{
			name:       "no builtin, verify block but non-bearer value: Unsupported",
			prov:       &provider.Provider{ID: "p5", Verify: &provider.VerifyConfig{Endpoint: "https://x.example.com"}},
			value:      StoreValue{KubeconfigYAML: "apiVersion: v1"},
			wantStatus: VerifyUnsupported,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			if tc.flow != nil {
				Register(tc.flow)
			}
			if tc.prov != nil && tc.prov.Verify != nil && tc.prov.Verify.Endpoint == "PLACEHOLDER" {
				tc.prov.Verify.Endpoint = srv.URL
			}
			res := VerifyCredential(context.Background(), tc.prov, tc.value)
			assert.Equal(t, tc.wantStatus, res.Status)
			if res.Status != VerifyValid {
				assert.NotEmpty(t, res.Detail, "no-silent-errors: every non-valid outcome carries a detail")
			}
			if tc.flow != nil && tc.flow.err == nil {
				require.NotNil(t, tc.flow.gotReq, "flow.Verify not called")
				assert.Equal(t, tc.value, tc.flow.gotReq.Value)
			}
		})
	}
}

// TestVerifyHTTPBearer_InvalidMethodIsIndeterminate covers the
// request-construction failure path: an invalid HTTP method token fails
// before any network call, and must map to Indeterminate (not a panic or
// a false Rejected).
func TestVerifyHTTPBearer_InvalidMethodIsIndeterminate(t *testing.T) {
	prov := &provider.Provider{
		ID: "p", Title: "P", Shape: "bearer",
		Verify: &provider.VerifyConfig{Endpoint: "https://api.example.com/user", Method: "BAD METHOD"},
	}
	res := VerifyHTTPBearer(context.Background(), prov, "tok")
	assert.Equal(t, VerifyIndeterminate, res.Status)
	assert.Contains(t, res.Detail, "building verification request failed")
}

// TestBodyMessage covers the JSON error-hint extractor, including the
// >200-char truncation branch used to bound a hostile/verbose provider body.
func TestBodyMessage(t *testing.T) {
	long := strings.Repeat("x", 250)
	cases := []struct {
		name  string
		body  string
		want  string // exact, when trunc is false
		trunc bool
	}{
		{name: "short message passes through", body: `{"message":"Bad credentials"}`, want: "Bad credentials"},
		{name: "invalid json → empty", body: `not json`, want: ""},
		{name: "no message field → empty", body: `{"other":"x"}`, want: ""},
		{name: "over-200 message truncated with an ellipsis", body: `{"message":"` + long + `"}`, trunc: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bodyMessage([]byte(tc.body))
			if tc.trunc {
				assert.True(t, strings.HasSuffix(got, "…"), "expected an ellipsis suffix, got %q", got)
				assert.Equal(t, 200+len("…"), len(got), "should keep 200 bytes + the ellipsis")
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// TestVerifyCredential_NormalizesEmptyStatusAndSynthesizesDetail covers the
// two defensive normalization branches: a flow that returns an empty Status
// is downgraded to Indeterminate, and any non-Valid result with an empty
// Detail gets a synthesized one (no-silent-errors — never a blank warning).
func TestVerifyCredential_NormalizesEmptyStatusAndSynthesizesDetail(t *testing.T) {
	cases := []struct {
		name       string
		result     VerifyResult
		wantStatus VerifyStatus
		wantDetail string // substring
	}{
		{
			name:       "empty status → normalized to Indeterminate + synthesized detail",
			result:     VerifyResult{},
			wantStatus: VerifyIndeterminate,
			wantDetail: "reported indeterminate with no detail",
		},
		{
			name:       "non-Valid status, empty detail → synthesized detail",
			result:     VerifyResult{Status: VerifyRejected},
			wantStatus: VerifyRejected,
			wantDetail: "reported rejected with no detail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			Register(&scriptedFlow{name: "norm-flow", result: tc.result})
			res := VerifyCredential(context.Background(),
				&provider.Provider{ID: "normprov", Builtin: "norm-flow"},
				StoreValue{Bearer: "tok"})
			assert.Equal(t, tc.wantStatus, res.Status)
			assert.Contains(t, res.Detail, tc.wantDetail)
		})
	}
}

// TestVerifyCredential_StampsTheProviderThatRanTheCheck — every arm of
// VerifyCredential must carry the provider's catalog id back to the caller.
//
// SubjectID alone is not an identity: it is stable only WITHIN a provider's
// id-space, and the caller that stores an attestation has to name both. A
// caller re-deriving the provider from the credential name is the failure this
// prevents — it can resolve a different provider than the one that verified.
//
// Every arm is covered, including the ones that report no subject at all: an
// attestation writer keys on "both halves present", so an arm that silently
// dropped the id would make a verified credential look unattested.
func TestVerifyCredential_StampsTheProviderThatRanTheCheck(t *testing.T) {
	installVerifyClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat","id":583231}`))
	}))
	t.Cleanup(srv.Close)

	cases := []struct {
		name          string
		prov          *provider.Provider
		flow          *scriptedFlow
		value         StoreValue
		wantProviderD string
		wantSubjectID string
	}{
		{
			name:          "declarative probe: provider id and stable subject id both come back",
			prov:          &provider.Provider{ID: "github-pat", Verify: &provider.VerifyConfig{Endpoint: "PLACEHOLDER", SubjectField: "login", SubjectIDField: "id"}},
			value:         StoreValue{Bearer: "tok"},
			wantProviderD: "github-pat",
			wantSubjectID: "583231",
		},
		{
			name:          "builtin flow: the id is stamped even though the flow never sets it",
			prov:          &provider.Provider{ID: "p-flow", Builtin: "flow-stamp"},
			flow:          &scriptedFlow{name: "flow-stamp", result: VerifyResult{Status: VerifyValid, Subject: "me", SubjectID: "42"}},
			value:         StoreValue{Bearer: "tok"},
			wantProviderD: "p-flow",
			wantSubjectID: "42",
		},
		{
			name:          "flow errored: the id still names who was asked",
			prov:          &provider.Provider{ID: "p-err", Builtin: "flow-err"},
			flow:          &scriptedFlow{name: "flow-err", err: fmt.Errorf("boom")},
			value:         StoreValue{Bearer: "tok"},
			wantProviderD: "p-err",
		},
		{
			name:          "unsupported: the id is stamped, so a caller can tell 'no check' from 'no provider'",
			prov:          &provider.Provider{ID: "p-none"},
			value:         StoreValue{Bearer: "tok"},
			wantProviderD: "p-none",
		},
		{
			name:  "nil provider: nothing to name, so nothing is claimed",
			value: StoreValue{Bearer: "tok"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			if tc.flow != nil {
				Register(tc.flow)
			}
			if tc.prov != nil && tc.prov.Verify != nil && tc.prov.Verify.Endpoint == "PLACEHOLDER" {
				tc.prov.Verify.Endpoint = srv.URL
			}
			res := VerifyCredential(context.Background(), tc.prov, tc.value)
			assert.Equal(t, tc.wantProviderD, res.ProviderID)
			assert.Equal(t, tc.wantSubjectID, res.SubjectID)
		})
	}
}
