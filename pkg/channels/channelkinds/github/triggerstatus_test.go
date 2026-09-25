package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

// The fixture pull request every case below reports on. Named once so a case
// asserting on a request path cannot disagree with the binding key that
// produced it.
const (
	fxOwner   = "demo-org"
	fxRepo    = "platform"
	fxPR      = 42
	fxHeadSHA = "0123456789abcdef0123456789abcdef01234567"
	fxAppSlug = "demo-reviewbot"
)

// recordedRequest is one call the fixture GitHub saw.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

// fakeGitHub is a stand-in for the four REST surfaces a check run needs:
// minting an installation token, reading the pull request's head, listing the
// check runs on that head, and writing one. Every request is recorded so a
// case can assert what actually went out — the only way to state "the
// identifiers came from the trigger, not from the caller".
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	// existing is what the list endpoint reports for the head SHA. Empty means
	// no check run has been opened for this commit yet.
	existing []map[string]any

	requests []recordedRequest
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{t: t}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_fixture_installation_token","expires_at":"2099-01-01T00:00:00Z"}`))
	})

	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"head":{"sha":"` + fxHeadSHA + `"}}`))
	})

	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		body := map[string]any{"check_runs": g.existing}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	})

	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":9001}`))
	})

	mux.HandleFunc("PATCH /repos/{owner}/{repo}/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":9001}`))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		t.Errorf("fakeGitHub: unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) record(r *http.Request) {
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
	if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			rec.Body = body
		}
	}
	g.requests = append(g.requests, rec)
}

// writes returns every request that changed something on the provider.
func (g *fakeGitHub) writes() []recordedRequest {
	var out []recordedRequest
	for _, r := range g.requests {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/check-runs") {
			out = append(out, r)
		}
		if r.Method == http.MethodPatch {
			out = append(out, r)
		}
	}
	return out
}

// testPEM generates a throwaway RSA key. The minter signs a real App JWT with
// it, so the fixture exercises the same signing leg production does rather than
// stubbing it out.
func testPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func fixtureChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewbot-github", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:   "github",
			Role:   spiceboxv1alpha1.ChannelRoleInput,
			GitHub: &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: fxAppSlug},
		},
	}
}

func fixtureBinding() *spiceboxv1alpha1.ChannelBinding {
	return &spiceboxv1alpha1.ChannelBinding{
		Name: "reviewbot-github",
		Kind: "github",
		Key:  "pr:" + fxOwner + "/" + fxRepo + "#42",
	}
}

func fixtureSecrets(t *testing.T) channelkinds.WebhookSecrets {
	t.Helper()
	return channelkinds.WebhookSecrets{Data: map[string][]byte{
		"app-id":          []byte("1"),
		"private-key":     testPEM(t),
		"installation-id": []byte("7"),
		"webhook-secret":  []byte("unused-here"),
	}}
}

// newSurface builds the surface under test against g.
func newSurface(t *testing.T, g *fakeGitHub) channelkinds.TriggerSurface {
	t.Helper()
	s, err := github.Kind{}.TriggerSurface(fixtureChannel(), fixtureBinding(), fixtureSecrets(t),
		channelkinds.TriggerStatusOptions{ProviderAPIBaseURL: g.srv.URL})
	require.NoError(t, err)
	require.NotNil(t, s)
	return s
}

// TestTriggerSurface_ClaimAddressesTheTriggersOwnIdentifiers is the property the
// whole seam exists for: nothing about WHICH pull request or WHICH commit is
// supplied by the caller — Claim takes no arguments at all — so the repository,
// the pull request number and the head SHA can only have come from the binding
// the webhook produced and from GitHub's own answer about that pull request.
func TestTriggerSurface_ClaimAddressesTheTriggersOwnIdentifiers(t *testing.T) {
	g := newFakeGitHub(t)
	claim, err := newSurface(t, g).Claim(context.Background())
	require.NoError(t, err)

	assert.False(t, claim.Concluded, "no check run existed for this head, so nothing was already answered")
	assert.NotEmpty(t, claim.Ref, "a claim must name the status it opened so a human can find it")

	writes := g.writes()
	require.Len(t, writes, 1, "claiming opens exactly one check run")
	assert.Equal(t, http.MethodPost, writes[0].Method)
	assert.Equal(t, "/repos/"+fxOwner+"/"+fxRepo+"/check-runs", writes[0].Path,
		"the repository comes from the binding key, not from an argument")
	assert.Equal(t, fxHeadSHA, writes[0].Body["head_sha"],
		"the head SHA comes from GitHub's answer about the pull request, not from an argument")
	assert.Equal(t, fxAppSlug, writes[0].Body["name"],
		"the check run is named for the App, so the same name is found again on the next delivery")
	assert.Equal(t, "in_progress", writes[0].Body["status"])

	// And the read leg addressed the pull request the binding named.
	var sawPRRead bool
	for _, r := range g.requests {
		if r.Method == http.MethodGet && r.Path == "/repos/"+fxOwner+"/"+fxRepo+"/pulls/42" {
			sawPRRead = true
		}
	}
	assert.True(t, sawPRRead, "the head SHA must be resolved from the pull request the binding names")
}

// TestTriggerSurface_ClaimReportsAnAlreadyConcludedRun is the redelivery case:
// GitHub already holds an answer for this commit, so claiming must report that
// and write nothing. This is the dedup fact an agent previously had to
// reconstruct with a hand-written list call.
func TestTriggerSurface_ClaimReportsAnAlreadyConcludedRun(t *testing.T) {
	cases := []struct {
		name       string
		conclusion string
		want       channelkinds.TriggerOutcome
	}{
		{name: "success reads back as clean", conclusion: "success", want: channelkinds.TriggerOutcomeClean},
		{name: "action_required reads back as problems_found", conclusion: "action_required", want: channelkinds.TriggerOutcomeProblemsFound},
		{name: "neutral reads back as could_not_finish", conclusion: "neutral", want: channelkinds.TriggerOutcomeCouldNotFinish},
		{name: "a conclusion this kind never writes still reads as could_not_finish", conclusion: "cancelled", want: channelkinds.TriggerOutcomeCouldNotFinish},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitHub(t)
			g.existing = []map[string]any{{
				"id": 4242, "name": fxAppSlug, "status": "completed", "conclusion": tc.conclusion,
			}}

			claim, err := newSurface(t, g).Claim(context.Background())
			require.NoError(t, err)
			assert.True(t, claim.Concluded)
			assert.Equal(t, tc.want, claim.Outcome)
			assert.Empty(t, g.writes(), "an already-answered commit must not be re-opened")
		})
	}
}

// TestTriggerSurface_ClaimIsIdempotent: a second delivery that lands while the
// first review is still running must not open a second check run — GitHub would
// then show two, and the next delivery could not tell which one it was reading.
func TestTriggerSurface_ClaimIsIdempotent(t *testing.T) {
	g := newFakeGitHub(t)
	g.existing = []map[string]any{{"id": 4242, "name": fxAppSlug, "status": "in_progress"}}

	claim, err := newSurface(t, g).Claim(context.Background())
	require.NoError(t, err)
	assert.False(t, claim.Concluded, "an in-progress run carries no answer yet")
	assert.Contains(t, claim.Ref, "4242", "the claim names the run already open")
	assert.Empty(t, g.writes(), "re-claiming writes nothing")
}

// TestTriggerSurface_ConcludeTargetsTheOpenClaim covers the mapping from the
// framework's outcome vocabulary onto github's, and pins that the conclusion
// lands on the run the claim opened rather than opening another.
func TestTriggerSurface_ConcludeTargetsTheOpenClaim(t *testing.T) {
	cases := []struct {
		name    string
		outcome channelkinds.TriggerOutcome
		want    string
	}{
		{name: "clean concludes success", outcome: channelkinds.TriggerOutcomeClean, want: "success"},
		{name: "problems_found concludes action_required", outcome: channelkinds.TriggerOutcomeProblemsFound, want: "action_required"},
		{name: "could_not_finish concludes neutral", outcome: channelkinds.TriggerOutcomeCouldNotFinish, want: "neutral"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitHub(t)
			g.existing = []map[string]any{{"id": 4242, "name": fxAppSlug, "status": "in_progress"}}

			err := newSurface(t, g).Conclude(context.Background(), channelkinds.TriggerConclusion{
				Outcome:    tc.outcome,
				Summary:    "Two findings, both in the auth path.",
				DetailsURL: "https://example.test/thread/1",
			})
			require.NoError(t, err)

			writes := g.writes()
			require.Len(t, writes, 1, "concluding updates the open run; it does not open a second")
			assert.Equal(t, http.MethodPatch, writes[0].Method)
			assert.Equal(t, "/repos/"+fxOwner+"/"+fxRepo+"/check-runs/4242", writes[0].Path)
			assert.Equal(t, "completed", writes[0].Body["status"])
			assert.Equal(t, tc.want, writes[0].Body["conclusion"])
			assert.Equal(t, fxHeadSHA, writes[0].Body["external_id"],
				"the commit this answer is about is recorded by the kind, not by the agent")
			assert.Equal(t, "https://example.test/thread/1", writes[0].Body["details_url"])
			output, _ := writes[0].Body["output"].(map[string]any)
			require.NotNil(t, output)
			assert.Equal(t, "Two findings, both in the auth path.", output["summary"])
		})
	}
}

// TestTriggerSurface_ConcludeWithNoClaimStillWritesTheAnswer is what makes
// claiming optional and stops a missed claim from losing a verdict: with
// nothing open, concluding opens a run that is already completed.
func TestTriggerSurface_ConcludeWithNoClaimStillWritesTheAnswer(t *testing.T) {
	g := newFakeGitHub(t)

	err := newSurface(t, g).Conclude(context.Background(), channelkinds.TriggerConclusion{
		Outcome: channelkinds.TriggerOutcomeClean,
		Summary: "Nothing blocking.",
	})
	require.NoError(t, err)

	writes := g.writes()
	require.Len(t, writes, 1)
	assert.Equal(t, http.MethodPost, writes[0].Method)
	assert.Equal(t, "completed", writes[0].Body["status"])
	assert.Equal(t, "success", writes[0].Body["conclusion"])
	assert.Equal(t, fxHeadSHA, writes[0].Body["head_sha"])
}

// TestTriggerSurface_ConcludeRefusesWhatItCannotPublish: the two arguments a
// caller does supply are the ones that reach a public pull request, so each is
// checked before anything goes out.
func TestTriggerSurface_ConcludeRefusesWhatItCannotPublish(t *testing.T) {
	cases := []struct {
		name    string
		in      channelkinds.TriggerConclusion
		wantErr string
	}{
		{
			name:    "a zero outcome is refused rather than defaulted",
			in:      channelkinds.TriggerConclusion{Summary: "done"},
			wantErr: "outcome",
		},
		{
			name:    "an outcome outside the closed set is refused",
			in:      channelkinds.TriggerConclusion{Outcome: "success", Summary: "done"},
			wantErr: "outcome",
		},
		{
			name:    "a non-http details URL is refused",
			in:      channelkinds.TriggerConclusion{Outcome: channelkinds.TriggerOutcomeClean, Summary: "done", DetailsURL: "javascript:alert(1)"},
			wantErr: "details URL",
		},
		{
			name:    "a relative details URL is refused",
			in:      channelkinds.TriggerConclusion{Outcome: channelkinds.TriggerOutcomeClean, Summary: "done", DetailsURL: "/orgs/demo-org"},
			wantErr: "details URL",
		},
		{
			name:    "an empty summary is refused",
			in:      channelkinds.TriggerConclusion{Outcome: channelkinds.TriggerOutcomeClean, Summary: "   "},
			wantErr: "summary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitHub(t)
			err := newSurface(t, g).Conclude(context.Background(), tc.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, g.writes(), "a refused conclusion must reach the provider not at all")
		})
	}
}

// TestTriggerSurface_SummaryIsBounded: the summary is agent-authored text
// published to a repository. An unbounded one is a cheap way to flood a pull
// request, and GitHub rejects an over-long body outright — which would turn a
// completed review into a stranded check run, the exact failure this seam
// exists to end.
func TestTriggerSurface_SummaryIsBounded(t *testing.T) {
	g := newFakeGitHub(t)
	err := newSurface(t, g).Conclude(context.Background(), channelkinds.TriggerConclusion{
		Outcome: channelkinds.TriggerOutcomeClean,
		Summary: strings.Repeat("x", 200_000),
	})
	require.NoError(t, err)

	writes := g.writes()
	require.Len(t, writes, 1)
	output, _ := writes[0].Body["output"].(map[string]any)
	require.NotNil(t, output)
	summary, _ := output["summary"].(string)
	assert.Less(t, len(summary), 200_000, "an over-long summary must be cut, not sent whole")
	assert.Contains(t, summary, "truncated", "the cut must be visible to whoever reads it")
}

// TestTriggerSurface_RefusedBeforeAnyIO covers the two shapes of "this binding
// exists but cannot be addressed". Both are errors rather than a nil surface:
// silently reporting "no trigger here" would leave a github session with no
// status surface and nothing said about why.
func TestTriggerSurface_RefusedBeforeAnyIO(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*spiceboxv1alpha1.ChannelBinding, channelkinds.WebhookSecrets)
		wantErr string
	}{
		{
			name:    "a binding key that is not a pull request",
			mutate:  func(b *spiceboxv1alpha1.ChannelBinding, _ channelkinds.WebhookSecrets) { b.Key = "thread:C123:1.2" },
			wantErr: "pull request",
		},
		{
			name: "a Secret with no installation id names the missing key",
			mutate: func(_ *spiceboxv1alpha1.ChannelBinding, s channelkinds.WebhookSecrets) {
				delete(s.Data, "installation-id")
			},
			wantErr: "installation-id",
		},
		{
			name: "a Secret with no private key names the missing key",
			mutate: func(_ *spiceboxv1alpha1.ChannelBinding, s channelkinds.WebhookSecrets) {
				delete(s.Data, "private-key")
			},
			wantErr: "private-key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureBinding()
			secrets := fixtureSecrets(t)
			tc.mutate(b, secrets)

			s, err := github.Kind{}.TriggerSurface(fixtureChannel(), b, secrets, channelkinds.TriggerStatusOptions{})
			require.Error(t, err)
			assert.Nil(t, s, "a refused surface must be nil, never a handle that fails later")
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "BEGIN RSA PRIVATE KEY",
				"no error from this package may carry key material")
		})
	}
}

// TestTriggerSurface_NoGitHubBlockIsRefusedNotIgnored: a Channel with no
// spec.github cannot name its App and so cannot name its check run. That is an
// ERROR rather than the seam's "no trigger here" silence — every github session
// has a pull request behind it, so reporting absence would leave the agent
// unable to answer and nobody told why.
func TestTriggerSurface_NoGitHubBlockIsRefusedNotIgnored(t *testing.T) {
	ch := fixtureChannel()
	ch.Spec.GitHub = nil
	s, err := github.Kind{}.TriggerSurface(ch, fixtureBinding(), fixtureSecrets(t), channelkinds.TriggerStatusOptions{})
	require.Error(t, err)
	assert.Nil(t, s)
	assert.Contains(t, err.Error(), "spec.github")
}

// TestTriggerSurfaceKind_IsPureCopy: the generic name is what tool assembly
// reads to describe the tools it is offering, from the binding's kind alone. It
// must therefore cost nothing and depend on nothing.
func TestTriggerSurfaceKind_IsPureCopy(t *testing.T) {
	assert.Contains(t, github.Kind{}.TriggerSurfaceKind(), "check run")
}

// TestTriggerSurface_SurfaceNamesSomethingAReaderCanGoLookAt: Surface() is copy
// that reaches a model's tool description and a person's notice, so it must
// name the pull request it is about rather than an internal handle.
func TestTriggerSurface_SurfaceNamesSomethingAReaderCanGoLookAt(t *testing.T) {
	g := newFakeGitHub(t)
	got := newSurface(t, g).Surface()
	assert.Contains(t, got, "check run")
	assert.Contains(t, got, fxOwner+"/"+fxRepo+"#42")
	assert.Empty(t, g.requests, "naming the surface must cost no provider call")
}
