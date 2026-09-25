//go:build e2e

// reviewbot_dedup.go wires the e2e harness for Task 6 (reviewbot dedup): a
// FakeGitHub stand-in for the three GitHub REST surfaces the review loop
// touches, two ExtraTools that stand in for the real toolspec-driven review
// loop's GitHub calls, and the Harness helpers reviewbot_test.go drives.
//
// Scope, stated honestly: this harness proves the TRANSPORT + SESSION
// CONTRACT end to end with real production code — webhook HMAC verification
// and translation (pkg/channels/channelkinds/github), the webd→channelsd NATS
// handoff (pkg/channels/channelevents, mirrored into channelsd's real
// pl.Deliver), channelKey→AgentSession correlation
// (pkg/channels/channelsd/pipeline), and the real kind=slack sender against
// fakeslack. What it does NOT exercise is the real reviewbot bundle's
// SpiceboxToolspec-driven `gh`/`git` sandbox execution or the inner
// Claude-Code bridge — those need a real sandbox pod and are Task 7's manual
// bring-up. The two ExtraTools below stand in for that inner loop's GitHub
// calls, making REAL HTTP requests to FakeGitHub so the dedup contract
// (GitHub's Check Run is the only "already reviewed" record) is exercised
// against a real wire boundary rather than an in-process shortcut.
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
	"github.com/authzed/openagentprimitives/pkg/web/webui/channelwebhook"
)

// Fixture constants shared between ApplyGitHubChannel's applied manifests,
// the two ExtraTools, and the JSON fixtures under testdata/reviewbot/. Kept
// as one set of names (never retyped) so the pieces cannot drift apart.
const (
	reviewbotOwner          = "demo-org"
	reviewbotRepo           = "platform"
	reviewbotPRNumber       = 42
	reviewbotCheckName      = "reviewbot"
	reviewbotSlackChannelID = "C-REVIEWBOT-DEDUP"
	reviewbotNamespace      = "default"
)

// ---------------------------------------------------------------------
// FakeGitHub
// ---------------------------------------------------------------------

// FakeGitHub is an httptest-backed stand-in for GitHub's REST API, scoped to
// the three surfaces the reviewbot dedup contract touches: minting an
// installation access token, listing check runs for a commit, and creating
// one — plus a PR read, which the review loop's tool call also exercises.
// Each is a DISTINCT handler: a fault or a bug in one must never mask the
// others, the same reason the MCP stub's own doc gives for not faulting
// every request identically.
//
// State is in-memory, keyed by owner/repo/sha, and is the ONLY place
// "already reviewed" is recorded — mirroring the real contract, where GitHub
// (not AP) holds the durable dedup fact.
type FakeGitHub struct {
	srv *httptest.Server

	mu        sync.Mutex
	checkRuns map[string][]fakeCheckRun
	// headSHA is what the pull-request read reports as the current head. The
	// trigger-status surface resolves the commit it answers for from THIS
	// rather than from anything the agent carried, so a scenario states the
	// commit here and nowhere else.
	headSHA string
	// nextRunID mints check run ids. Real ones, because the kind patches a run
	// by id: a fixture handing back 0 would let a broken id round trip pass.
	nextRunID int64
	// mintRunID, when non-nil, hands back the ids a CAPTURED run saw GitHub
	// mint, in mint order, instead of this fixture's own counter.
	//
	// The whole of tier 2 in one field. A captured session's recorded reply
	// names the id GitHub chose, and a step's divergence check was derived from
	// those exact bytes — so a fixture minting 1 where the recording says
	// 99044729080 diverges on a run that took the identical path. Seeding the
	// number the provider chose is what lets everything around it stay real:
	// the claim, the create, the find-by-name, and the patch-BY-ID all still
	// execute against this server over HTTP.
	//
	// It pins ONLY the number. Nothing else about the round trip is served from
	// a recording, which is why this is not the "can the reply" move the bundle
	// format refuses.
	mintRunID func() string

	installCalls        int
	prReadCalls         int
	checkRunListCalls   int
	checkRunCreateCalls int
	checkRunUpdateCalls int
}

type fakeCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ExternalID string `json:"external_id,omitempty"`
	DetailsURL string `json:"details_url,omitempty"`
	Output     *struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output,omitempty"`
}

// newFakeGitHub starts the stub and registers a t.Cleanup to close it.
func newFakeGitHub(t *testing.T) *FakeGitHub {
	t.Helper()
	g := &FakeGitHub{checkRuns: map[string][]fakeCheckRun{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", g.handleMintToken)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", g.handlePRRead)
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", g.handleListCheckRuns)
	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs", g.handleCreateCheckRun)
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/check-runs/{id}", g.handleUpdateCheckRun)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// URL is the stub's base address: pass to githubapp.WithBaseURL, or build a
// REST path directly against it.
func (g *FakeGitHub) URL() string { return g.srv.URL }

// handleMintToken serves the installation-access-token exchange
// (POST /app/installations/{id}/access_tokens), the second leg of
// githubapp.HTTPMinter.Mint. It does not verify the bearer App JWT — that is
// GitHub's own job in production; this fixture only needs to hand back a
// token an authenticated caller can present to the other two endpoints.
func (g *FakeGitHub) handleMintToken(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	g.installCalls++
	g.mu.Unlock()
	resp := struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}{Token: "ghs-fake-installation-token", ExpiresAt: time.Now().Add(time.Hour)}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handlePRRead serves GET /repos/{owner}/{repo}/pulls/{number} — the review
// loop's context-gathering read. Its result is not consulted for the
// reviewed/not-reviewed decision (the check-run list is the dedup source of
// truth); it exists so the "PR reads" surface the brief calls for is a real,
// distinct handler rather than folded into the check-run list.
func (g *FakeGitHub) handlePRRead(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.prReadCalls++
	head := g.headSHA
	g.mu.Unlock()
	resp := map[string]any{
		"number": r.PathValue("number"),
		"state":  "open",
		"title":  "Add rate limiting to the ingest handler",
		// The current head commit. The trigger-status surface reads it here,
		// which is what lets a session that spans several pushes answer for the
		// commit in front of it rather than the one it was created on.
		"head": map[string]any{"sha": head},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func checkRunKey(owner, repo, sha string) string { return owner + "/" + repo + "@" + sha }

// handleListCheckRuns serves GET /repos/{owner}/{repo}/commits/{sha}/check-runs.
// This IS the dedup read: the review loop calls it before doing any work, and
// a completed "reviewbot" run in the response means "already reviewed".
//
// The check_name query parameter is honored, because the github kind sends it
// and re-checks the name on the way back. Serving it unfiltered would let a
// filter bug pass here and reach a busy commit in production.
func (g *FakeGitHub) handleListCheckRuns(w http.ResponseWriter, r *http.Request) {
	owner, repo, sha := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("sha")
	g.mu.Lock()
	g.checkRunListCalls++
	runs := append([]fakeCheckRun(nil), g.checkRuns[checkRunKey(owner, repo, sha)]...)
	g.mu.Unlock()
	if want := r.URL.Query().Get("check_name"); want != "" {
		filtered := runs[:0]
		for _, run := range runs {
			if run.Name == want {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}
	resp := map[string]any{"total_count": len(runs), "check_runs": runs}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleCreateCheckRun serves POST /repos/{owner}/{repo}/check-runs. This IS
// the dedup write: the ordering constraint under test requires the review
// loop to call it LAST, after the Slack post has already gone out.
func (g *FakeGitHub) handleCreateCheckRun(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var cr fakeCheckRun
	if err := json.Unmarshal(body, &cr); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.checkRunCreateCalls++
	cr.ID = g.mintCheckRunID()
	key := checkRunKey(owner, repo, cr.HeadSHA)
	g.checkRuns[key] = upsertCheckRun(g.checkRuns[key], cr)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(cr)
}

// handleUpdateCheckRun serves PATCH /repos/{owner}/{repo}/check-runs/{id} —
// how a claimed run is concluded. Addressed BY ID, so a caller that lost track
// of which run it opened cannot land here at all; that is the property the
// 404 below keeps honest.
func (g *FakeGitHub) handleUpdateCheckRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var patch fakeCheckRun
	if err := json.Unmarshal(body, &patch); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRunUpdateCalls++
	for key, runs := range g.checkRuns {
		for i, run := range runs {
			if run.ID != id {
				continue
			}
			// A PATCH carries no name or head_sha; github derives both from
			// the run being patched, so the fixture must too.
			patch.ID, patch.Name, patch.HeadSHA = run.ID, run.Name, run.HeadSHA
			g.checkRuns[key][i] = patch
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(patch)
			return
		}
	}
	http.Error(w, "no such check run", http.StatusNotFound)
}

// CheckRunFor returns the check run named name on sha in this fixture's fixed
// demo-org/platform repo, or nil. The assertion surface for what the github
// kind actually published.
func (g *FakeGitHub) CheckRunFor(sha, name string) *fakeCheckRun {
	return g.CheckRunIn(reviewbotOwner, reviewbotRepo, sha, name)
}

// CheckRunIn is CheckRunFor against a named repository, for a fixture that
// ships its own Channel and therefore its own owner/repo rather than sharing
// this file's constants.
func (g *FakeGitHub) CheckRunIn(owner, repo, sha, name string) *fakeCheckRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, run := range g.checkRuns[checkRunKey(owner, repo, sha)] {
		if run.Name == name {
			out := run
			return &out
		}
	}
	return nil
}

// SetHeadSHA is what the pull-request read reports as the current head commit.
func (g *FakeGitHub) SetHeadSHA(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.headSHA = sha
}

// SetCheckRunIDMinter makes this fixture hand back the ids a captured run saw
// the real provider mint, in mint order, instead of its own counter.
//
// mint returns the id as a STRING because that is how a bundle carries it and
// how bt.MintedIDSequence hands one out — one spelling of the value from the
// record to the wire. A value that is not a positive integer falls back to the
// counter and says so on the fixture's own error surface rather than serving
// zero, which is the id the fixture's counter comment already explains must
// never reach the kind.
//
// nil (the default) leaves the counter in place, which is what every authored
// bundle wants: it makes no claim about what the provider named its runs.
func (g *FakeGitHub) SetCheckRunIDMinter(mint func() string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mintRunID = mint
}

// mintCheckRunID is the one place a check run gets its id. Callers hold g.mu.
//
// The seeded minter wins when there is one, and the counter is the fallback for
// both "no bundle pinned any" and "the pinned value was unusable" — never 0,
// which would let a broken patch-by-id round trip pass.
func (g *FakeGitHub) mintCheckRunID() int64 {
	if g.mintRunID != nil {
		if id, err := strconv.ParseInt(g.mintRunID(), 10, 64); err == nil && id > 0 {
			return id
		}
		// Falling through is deliberate and is not silent: the sequence's own
		// report func is what fires on exhaustion, naming the family and the
		// index, so the run fails by name rather than on whatever the counter
		// happened to produce.
	}
	g.nextRunID++
	return g.nextRunID
}

// CheckRunCalls counts the writes this fixture saw. Creates and updates are
// counted separately because the difference is the claim contract: a claim
// followed by a conclusion is one create and one update, while two creates
// would mean the conclusion lost track of the run it opened and left the claim
// behind — the stranded-check-run failure in miniature.
func (g *FakeGitHub) CheckRunCalls() (creates, updates int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checkRunCreateCalls, g.checkRunUpdateCalls
}

func upsertCheckRun(existing []fakeCheckRun, cr fakeCheckRun) []fakeCheckRun {
	for i, e := range existing {
		if e.Name == cr.Name {
			// Keep the id the run already had when the incoming write carries
			// none: a caller that re-creates over an existing run must not be
			// able to renumber it out from under a concurrent patch.
			if cr.ID == 0 {
				cr.ID = e.ID
			}
			existing[i] = cr
			return existing
		}
	}
	return append(existing, cr)
}

// SetCheckRunConclusion records that GitHub reports a completed check run
// named `name` with the given conclusion for `sha`, in this fixture's fixed
// demo-org/platform repo. This is the ONLY place "already reviewed" lives —
// mirroring the real contract, where GitHub (not AP) is the durable record.
// Safe to call even when the review loop's own post_review_check_run call
// already wrote the same fact; the write is an upsert.
func (g *FakeGitHub) SetCheckRunConclusion(sha, name, conclusion string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := checkRunKey(reviewbotOwner, reviewbotRepo, sha)
	// The plain counter, never the seeded minter: this stands for a run some
	// EARLIER delivery left behind, not one the session under test opened.
	// Drawing from the sequence here would consume an id the run's own create
	// is going to need and shift every later one by a position.
	g.nextRunID++
	g.checkRuns[key] = upsertCheckRun(g.checkRuns[key], fakeCheckRun{
		ID: g.nextRunID, Name: name, HeadSHA: sha, Status: "completed", Conclusion: conclusion,
	})
}

// ---------------------------------------------------------------------
// ExtraTools: stand-ins for the real toolspec-driven review loop's GitHub
// calls. Registered on the in-process runner factory's ExtraTools seam
// (see secret_output_producer.go / sre_pinning_producer.go for the same
// pattern) so the ScriptedLLM script drives a REAL tool_use/tool_result
// round trip, and Execute makes REAL HTTP calls to FakeGitHub rather than
// consulting it in-process.
// ---------------------------------------------------------------------

// fixtureRSAKeyPEM lazily generates one RSA keypair for the whole test
// binary, PEM-encoded, so postReviewCheckRunTool's githubapp.HTTPMinter has
// something to sign an App JWT with. FakeGitHub never verifies the
// signature — the minter's OWN JWT-signing code path is what this exercises,
// not GitHub's verification of it.
var fixtureRSAKeyPEM = sync.OnceValue(func() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("reviewbot e2e fixture: generate RSA key: %v", err))
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
})

// checkReviewStatusTool is the dedup READ: it reads the PR (a distinct
// FakeGitHub handler) and lists check runs for the given head SHA, reporting
// whether a completed "reviewbot" run already exists.
type checkReviewStatusTool struct {
	gh *FakeGitHub
}

func (c *checkReviewStatusTool) Name() string    { return "check_review_status" }
func (c *checkReviewStatusTool) Kind() tool.Kind { return tool.KindSandbox }
func (c *checkReviewStatusTool) Description() string {
	return "Reads the pull request and lists GitHub check runs for the head SHA, reporting whether reviewbot has already posted a completed review for it."
}
func (c *checkReviewStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["sha"],"properties":{"sha":{"type":"string"},"_reason":{"type":"string"}}}`)
}

func (c *checkReviewStatusTool) Execute(ctx context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var in struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.SHA == "" {
		return tool.Result{Content: fmt.Sprintf("check_review_status: bad args: %v", err), IsError: true}, nil
	}

	// PR read: a real request to FakeGitHub's distinct pulls handler.
	// Best-effort context, not consulted for the reviewed/not-reviewed
	// decision — same as the real review loop, whose PR read informs the
	// write-up but never gates dedup.
	prURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.gh.URL(), reviewbotOwner, reviewbotRepo, reviewbotPRNumber)
	prReq, err := http.NewRequestWithContext(ctx, http.MethodGet, prURL, nil)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("check_review_status: build PR read request: %v", err), IsError: true}, nil
	}
	prResp, err := http.DefaultClient.Do(prReq)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("check_review_status: PR read failed: %v", err), IsError: true}, nil
	}
	_ = prResp.Body.Close()

	listURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", c.gh.URL(), reviewbotOwner, reviewbotRepo, in.SHA)
	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("check_review_status: build check-runs request: %v", err), IsError: true}, nil
	}
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("check_review_status: check-runs list failed: %v", err), IsError: true}, nil
	}
	defer func() { _ = listResp.Body.Close() }()
	var parsed struct {
		CheckRuns []fakeCheckRun `json:"check_runs"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&parsed); err != nil {
		return tool.Result{Content: fmt.Sprintf("check_review_status: decode check-runs: %v", err), IsError: true}, nil
	}
	reviewed := false
	for _, cr := range parsed.CheckRuns {
		if cr.Name == reviewbotCheckName && cr.Status == "completed" {
			reviewed = true
		}
	}
	out, _ := json.Marshal(map[string]any{"already_reviewed": reviewed, "sha": in.SHA})
	return tool.Result{Content: string(out)}, nil
}

func (c *checkReviewStatusTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (c *checkReviewStatusTool) PermissionVariants() []authz.PermissionVariant { return nil }

var _ tool.Tool = (*checkReviewStatusTool)(nil)

// postReviewCheckRunTool is the dedup WRITE: it mints a GitHub App
// installation token (the real githubapp.HTTPMinter, pointed at FakeGitHub)
// and creates the completed "reviewbot" Check Run for the head SHA. The
// AgentClass fixture's system prompt (ApplyGitHubChannel) — and every
// ScriptedLLM script driving this tool in reviewbot_test.go — call it ONLY
// after respond_to_user has already delivered the review: completing the
// Check Run first and then failing to deliver would leave a green check for
// a review nobody received, and since the Check Run IS the dedup marker,
// that review would never be retried.
type postReviewCheckRunTool struct {
	gh *FakeGitHub
}

func (p *postReviewCheckRunTool) Name() string    { return "post_review_check_run" }
func (p *postReviewCheckRunTool) Kind() tool.Kind { return tool.KindSandbox }
func (p *postReviewCheckRunTool) Description() string {
	return "Mints a GitHub App installation token and creates the completed reviewbot Check Run for the head SHA. Call ONLY after the review has already been delivered via respond_to_user."
}
func (p *postReviewCheckRunTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["sha","conclusion"],"properties":{"sha":{"type":"string"},"conclusion":{"type":"string"},"_reason":{"type":"string"}}}`)
}

func (p *postReviewCheckRunTool) Execute(ctx context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var in struct {
		SHA        string `json:"sha"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.SHA == "" {
		return tool.Result{Content: fmt.Sprintf("post_review_check_run: bad args: %v", err), IsError: true}, nil
	}
	if in.Conclusion == "" {
		in.Conclusion = "success"
	}

	minter := githubapp.NewHTTPMinter(githubapp.WithBaseURL(p.gh.URL()))
	token, err := minter.Mint(ctx, githubapp.MintRequest{
		AppID:          "1",
		PrivateKeyPEM:  fixtureRSAKeyPEM(),
		InstallationID: "1",
	})
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("post_review_check_run: mint installation token: %v", err), IsError: true}, nil
	}

	body, err := json.Marshal(fakeCheckRun{
		Name: reviewbotCheckName, HeadSHA: in.SHA, Status: "completed", Conclusion: in.Conclusion,
	})
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("post_review_check_run: marshal body: %v", err), IsError: true}, nil
	}
	createURL := fmt.Sprintf("%s/repos/%s/%s/check-runs", p.gh.URL(), reviewbotOwner, reviewbotRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(body))
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("post_review_check_run: build request: %v", err), IsError: true}, nil
	}
	req.Header.Set("Authorization", "Bearer "+string(token.AccessToken.UnderlyingValue()))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("post_review_check_run: create check run: %v", err), IsError: true}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return tool.Result{Content: fmt.Sprintf("post_review_check_run: unexpected status %d", resp.StatusCode), IsError: true}, nil
	}
	out, _ := json.Marshal(map[string]any{"created": true, "sha": in.SHA, "conclusion": in.Conclusion})
	return tool.Result{Content: string(out)}, nil
}

func (p *postReviewCheckRunTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (p *postReviewCheckRunTool) PermissionVariants() []authz.PermissionVariant { return nil }

var _ tool.Tool = (*postReviewCheckRunTool)(nil)

// ---------------------------------------------------------------------
// webhook_inbound NATS handoff (channelsd side) — see the subscription
// wired in harness.go's startChannelsdPlumbing.
// ---------------------------------------------------------------------

// webhookInboundNATSHandler mirrors internal/cmd/channelsd/webhook_inbound.go
// exactly (that file is package main and cannot be imported from here): it
// resolves the Channel CR named by a verified webhook delivery and dispatches
// it through the same inbound pipeline every other channelsd subscription in
// this harness uses. There is no caller to return an error to (this runs off
// a NATS message), so every failure path logs to stderr and returns; nothing
// here is silent.
//
// Deliberately no requeue or retry: a verified delivery that cannot be
// dispatched is logged and dropped, matching production's own rationale
// (GitHub's webhook retry is the recovery mechanism; webd already answers 503
// — which GitHub retries — for the one case worth retrying).
func webhookInboundNATSHandler(cli client.Client, pl *pipeline.Pipeline) func(*nats.Msg) {
	return func(m *nats.Msg) {
		var p channelevents.WebhookInboundPayload
		if err := json.Unmarshal(m.Data, &p); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: webhook_inbound: undecodable payload: %v\n", err)
			return
		}
		ctx := context.Background()
		var ch spiceboxv1alpha1.Channel
		if err := cli.Get(ctx, client.ObjectKey{Namespace: p.ChannelNamespace, Name: p.ChannelName}, &ch); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: webhook_inbound: Channel %s/%s lookup failed: %v\n", p.ChannelNamespace, p.ChannelName, err)
			return
		}
		// Trigger-owner derivation, mirrored from the production consumer
		// (internal/cmd/channelsd/webhook_inbound.go) like everything else in
		// this handler: without it, every triggered e2e session would silently
		// lack the PR author's owner annotation and no bundle could exercise
		// the additional-owner write.
		var preTurn map[string]string
		if k, ok := chregistry.Get(ch.Spec.Kind); ok {
			if op, ok := k.WebhookReceiver(channelkinds.Deps{}).(channelkinds.TriggerOwnerProvider); ok {
				subject, has, oerr := op.TriggerOwnerSubject(&ch, p.DeliveryEvent, p.RawDelivery)
				switch {
				case oerr != nil:
					fmt.Fprintf(os.Stderr, "e2e: webhook_inbound: trigger owner derivation failed for %s/%s key=%s: %v\n",
						p.ChannelNamespace, p.ChannelName, p.ChannelKey, oerr)
				case has:
					preTurn = map[string]string{spiceboxv1alpha1.AnnotationTriggerOwnerSubject: subject}
				}
			}
		}
		dec, err := pl.Deliver(ctx, channelkinds.InboundEvent{
			Channel:      &ch,
			ChannelKey:   p.ChannelKey,
			MessageText:  p.MessageText,
			AuthzSubject: p.AuthzSubject,
			// RawDelivery + DeliveryEvent must be carried here exactly as the
			// production consumer does (internal/cmd/channelsd/webhook_inbound.go:52-53):
			// the pipeline's trigger-fact block is gated on len(ev.RawDelivery)>0, so
			// dropping them silently disables envelope-fact derivation for every
			// triggered session — a precondition over facts.envelope.* reads
			// undetermined and no bundle can exercise the gate.
			RawDelivery:        p.RawDelivery,
			DeliveryEvent:      p.DeliveryEvent,
			PreTurnAnnotations: preTurn,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: webhook_inbound: pipeline.Deliver(%s/%s key=%s) errored: %v\n",
				p.ChannelNamespace, p.ChannelName, p.ChannelKey, err)
			return
		}
		if dec.Outcome == channelkinds.OutcomeInternalError {
			fmt.Fprintf(os.Stderr, "e2e: webhook_inbound: pipeline returned InternalError for %s/%s key=%s\n",
				p.ChannelNamespace, p.ChannelName, p.ChannelKey)
		}
	}
}

// ---------------------------------------------------------------------
// Harness helpers driven by reviewbot_test.go
// ---------------------------------------------------------------------

// ApplyGitHubChannel applies a self-contained reviewbot fixture: an
// AgentClass driven by the harness's ScriptedLLM, a kind=github input
// Channel bound to it, and a paired kind=slack output Channel — "a reply
// goes out on the paired output Channel", per the github Kind's own doc.
// Also lazily constructs h.FakeGitHub and the real production webhook HTTP
// handler (pkg/web/webui/channelwebhook), and registers the two ExtraTools
// (check_review_status, post_review_check_run) that stand in for the real
// toolspec-driven review loop's GitHub calls.
//
// Returns the applied github Channel. Call once per test, before posting any
// webhook — the ExtraTools registration and webhook-handler construction
// must land before the first AgentSession spawns.
func (h *Harness) ApplyGitHubChannel(t *testing.T, channelName, agentClassName string) *spiceboxv1alpha1.Channel {
	t.Helper()

	h.wireGitHubTransport(t)

	h.runnerFactory.ExtraTools = append(h.runnerFactory.ExtraTools,
		&checkReviewStatusTool{gh: h.FakeGitHub},
		&postReviewCheckRunTool{gh: h.FakeGitHub},
	)

	outputChannelName := channelName + "-slack-out"
	secretName := channelName + "-creds"
	slackSecretName := channelName + "-slack-creds"
	llmSecretName := agentClassName + "-llm-creds"

	yaml := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %[1]s
  namespace: %[9]s
type: Opaque
stringData:
  api-key: "unused-by-the-scripted-llm"
---
apiVersion: v1
kind: Secret
metadata:
  name: %[2]s
  namespace: %[9]s
type: Opaque
stringData:
  app-id: "1"
  private-key: "unused-by-this-fixture; the review-loop tool mints its own key"
  installation-id: "1"
  webhook-secret: %[7]q
---
apiVersion: v1
kind: Secret
metadata:
  name: %[3]s
  namespace: %[9]s
type: Opaque
stringData:
  bot-token: "xoxb-test"
  app-token: "xapp-test"
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: %[4]s
  namespace: %[9]s
spec:
  model:
    provider: anthropic
    name: claude-opus-4-7
    apiKey:
      name: %[1]s
      key: api-key
  systemPrompt:
    inline: |
      You review GitHub pull requests. On every inbound pull_request event:
        1. Call check_review_status for the event's head SHA.
        2. If already_reviewed is true, do nothing further this round — GitHub's
           own Check Run is the complete record, and a second review would post
           a duplicate to the same Slack thread.
        3. Otherwise, review the change and deliver your findings via
           respond_to_user.
        4. Only THEN call post_review_check_run for the head SHA. The Check Run
           must complete LAST, after Slack delivery: completing it first and
           then failing to deliver would leave a green check for a review
           nobody received, and since the Check Run is the only dedup marker,
           that review would never be retried.
        5. Finally, call conclude_trigger_status with the verdict. That is a
           different write from step 4 and neither replaces the other: step 4
           records THIS agent's dedup marker, which is a check run it names
           itself and which nothing in the framework can see, while
           conclude_trigger_status answers the event that started the round on
           the App's own check run. The requirement this class declares —
           trigger-status-concluded — reads the latter, so a round that stops
           after step 4 has left the pull request unanswered as far as anything
           outside this agent's own tools can tell.
  authz:
    toolCalls:
      mode: disabled
    session:
      # github's input Channel has UserAttributable=false (a PR author has no
      # AP identity), so the AgentClass controller REQUIRES this: without it
      # agentsession#interact would resolve to nothing for every session this
      # class spawns. No test in this package drives an interactive click
      # through it — it exists to satisfy the Valid=True gate honestly,
      # naming a plausible fixture group rather than a value chosen to dodge
      # the check.
      interactPermission: "group:%[4]s-maintainers#member"
  # The step whose omission is invisible from inside the session: the review is
  # delivered, the agent is satisfied, and the pull request goes on showing the
  # work as in progress. Declared here rather than only in the shipped bundle so
  # the class these scenarios drive is the same shape as the real one.
  completionRequirements:
    - trigger-status-concluded
  budget:
    maxTurns: 20
    maxTokens: 200000
    maxDuration: "10m"
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: Channel
metadata:
  name: %[5]s
  namespace: %[9]s
spec:
  kind: github
  role: input
  agentClass: %[4]s
  sessionScope: thread
  authzSubject: "service:%[4]s"
  owner:
    explicit: "user:%[4]s-owner"
  credentialsRef:
    secretName: %[2]s
  github:
    appSlug: %[4]s
    events: ["opened", "synchronize", "reopened", "ready_for_review"]
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: Channel
metadata:
  name: %[6]s
  namespace: %[9]s
spec:
  kind: slack
  role: output
  agentClass: %[4]s
  credentialsRef:
    secretName: %[3]s
  slack:
    mode: socket
    outputDefaults:
      channelId: %[8]q
      threadStrategy: new-thread-per-session
`, llmSecretName, secretName, slackSecretName, agentClassName, channelName, outputChannelName,
		reviewbotWebhookSecret, reviewbotSlackChannelID, reviewbotNamespace)

	h.ApplyManifest(yaml)

	h.WireGitHubCredentials(t, reviewbotNamespace, secretName)

	h.WaitForAgentClassValid(agentClassName, 30*time.Second)

	var ch spiceboxv1alpha1.Channel
	if err := h.K8s.Get(context.Background(), client.ObjectKey{Namespace: reviewbotNamespace, Name: channelName}, &ch); err != nil {
		t.Fatalf("ApplyGitHubChannel: get Channel %s: %v", channelName, err)
	}
	return &ch
}

// wireGitHubTransport stands up the process-wide halves of a github-triggered
// scenario: the fixture GitHub API, the production webhook route handler, and
// the runner's pointer at the former.
//
// Separate from ApplyGitHubChannel because a bundle brings its OWN manifests —
// its Channels, its AgentClass, its toolspecs are the fixture under test — and
// needs only this transport wiring. Idempotent, so a scenario that applies
// several github Channels still gets one FakeGitHub and one handler.
func (h *Harness) wireGitHubTransport(t *testing.T) {
	t.Helper()
	if h.FakeGitHub == nil {
		h.FakeGitHub = newFakeGitHub(t)
	}
	if h.githubWebhookHandler == nil {
		pub := &natsPublisher{nc: h.nc}
		wh, err := channelwebhook.New(h.K8s, pub.Publish)
		if err != nil {
			t.Fatalf("wireGitHubTransport: construct webhook handler: %v", err)
		}
		routes := wh.Routes(nil)
		if len(routes) != 1 {
			t.Fatalf("wireGitHubTransport: channelwebhook.Routes() = %d routes, want 1", len(routes))
		}
		h.githubWebhookHandler = routes[0].Handler
	}
	// Point the trigger-status meta tools at the fixture GitHub. Without it the
	// github kind would resolve its own default and a scenario would reach the
	// real api.github.com; production leaves this empty.
	h.runnerFactory.TriggerStatusAPIBaseURL = h.FakeGitHub.URL()
}

// WireGitHubCredentials prepares an already-applied kind=github Channel for
// signed delivery: it stands up the fixture transport (above) and patches a
// REAL RSA private key plus the harness webhook secret into the named
// credentials Secret.
//
// Both values are patched rather than written by the fixture's own YAML. The
// App private key has to be a real key — the trigger-status surface mints its
// installation token from this Secret, so a placeholder fails at JWT signing
// with an error nothing in a scenario could trace back to the fixture — and a
// PEM does not survive an indented heredoc, nor belongs checked in beside a
// bundle. The webhook secret is patched for the same reason SignWebhook reads
// it back rather than closing over the constant: one writer, one reader, one
// place they can disagree.
//
// A merge PATCH rather than a read-modify-write: the apply that created this
// Secret has only just landed, so a Get through the manager's cache can hand
// back a stale resourceVersion and the Update loses a conflict race it has no
// reason to enter.
func (h *Harness) WireGitHubCredentials(t *testing.T, ns, secretName string) {
	t.Helper()
	h.wireGitHubTransport(t)

	patch := fmt.Sprintf(`{"data":{"private-key":%q,"webhook-secret":%q}}`,
		base64.StdEncoding.EncodeToString(fixtureRSAKeyPEM()),
		base64.StdEncoding.EncodeToString([]byte(reviewbotWebhookSecret)))
	if err := h.K8s.Patch(context.Background(),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secretName}},
		client.RawPatch(types.MergePatchType, []byte(patch)),
	); err != nil {
		t.Fatalf("WireGitHubCredentials: patch github App credentials into Secret %s/%s: %v", ns, secretName, err)
	}
}

// WireGitHubAppIdentity makes a type=githubApp AgentIdentity credential
// RESOLVABLE at replay: it stands up the fixture transport, points a real
// githubapp.HTTPMinter at it, and patches usable App material into the named
// Secret.
//
// # Why a minter and not a rewritten credential
//
// A minted credential reads nothing from the Secret a fixture rewrite emits
// beside it. The placeholder satisfies the AgentIdentity's own Secret-exists
// check, so the replayed identity goes Valid and every tool call drawing on the
// credential fails at dispatch — an identity reporting healthy while nothing
// works. Rewriting it to a stored type would let the bundle run and would make
// it evidence about a different gate: Minted() is read by the broker's expiry
// rule and by identity-scoped token grants.
//
// With this wired, none of that is stood in for. githubapp's own code signs the
// App JWT and exchanges it for an installation token over HTTP against
// FakeGitHub, and the broker caches it against the expiry the minter returned.
// The only fiction is which server answered.
//
// The private key must be REAL — JWT signing happens in production code before
// any request is made, and a placeholder fails there with an error nothing in a
// scenario could trace back to the fixture. app-id and installation-id are left
// as whatever the fixture already carries: FakeGitHub does not verify the
// bearer JWT (that is GitHub's job in production), and the installation id only
// has to be a non-empty path segment.
func (h *Harness) WireGitHubAppIdentity(t *testing.T, ns, secretName string) {
	t.Helper()
	h.wireGitHubTransport(t)

	h.SetGitHubAppMinter(githubapp.Adapt(githubapp.NewHTTPMinter(
		githubapp.WithBaseURL(h.FakeGitHub.URL()),
	)))

	patch := fmt.Sprintf(`{"data":{"private-key":%q}}`,
		base64.StdEncoding.EncodeToString(fixtureRSAKeyPEM()))
	if err := h.K8s.Patch(context.Background(),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secretName}},
		client.RawPatch(types.MergePatchType, []byte(patch)),
	); err != nil {
		t.Fatalf("WireGitHubAppIdentity: patch App private key into Secret %s/%s: %v", ns, secretName, err)
	}
}

// reviewbotWebhookSecret is the shared HMAC secret WireGitHubCredentials
// patches into a github Channel's credentials Secret. SignWebhook reads the
// secret back from that same Secret (not this constant) so it stays honest
// about what receiver.Verify actually checks against; this constant exists only
// to seed it in one place.
const reviewbotWebhookSecret = "reviewbot-e2e-webhook-secret"

// LoadFixture reads a JSON webhook payload from testdata/reviewbot/<name>.
func (h *Harness) LoadFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join(TestdataDir("reviewbot"), name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("LoadFixture(%s): %v", name, err)
	}
	return data
}

// SignWebhook computes the HMAC-SHA256 signature GitHub's
// X-Hub-Signature-256 header carries, over body, using the webhook-secret
// key of ch's OWN credentials Secret (read fresh, not a cached constant) —
// exactly what pkg/channels/channelkinds/github's receiver.Verify checks
// against.
func (h *Harness) SignWebhook(t *testing.T, ch *spiceboxv1alpha1.Channel, body []byte) string {
	t.Helper()
	var sec corev1.Secret
	if err := h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: ch.Namespace, Name: ch.Spec.CredentialsRef.SecretName}, &sec); err != nil {
		t.Fatalf("SignWebhook: get credentials Secret %s/%s: %v", ch.Namespace, ch.Spec.CredentialsRef.SecretName, err)
	}
	secret := sec.Data["webhook-secret"]
	if len(secret) == 0 {
		t.Fatalf("SignWebhook: Secret %s/%s has no webhook-secret key", ch.Namespace, ch.Spec.CredentialsRef.SecretName)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// PostWebhook delivers body+signature to the REAL production webhook route
// handler (pkg/web/webui/channelwebhook), mounted over an
// httptest.NewRequest/NewRecorder pair rather than a live listening server —
// the handler itself is the genuine article, only the transport is
// in-process. ApplyGitHubChannel must have run first (it constructs
// h.githubWebhookHandler).
func (h *Harness) PostWebhook(t *testing.T, ch *spiceboxv1alpha1.Channel, body []byte, signature string) *http.Response {
	t.Helper()
	return h.PostWebhookEvent(t, ch, "pull_request", body, signature)
}

// PostWebhookEvent is PostWebhook with the provider's event-type header named
// explicitly. The receiver dispatches on that header and a payload alone does
// not carry it, so a scenario covering a second event type has to be able to
// say which one it is delivering.
func (h *Harness) PostWebhookEvent(t *testing.T, ch *spiceboxv1alpha1.Channel, event string, body []byte, signature string) *http.Response {
	t.Helper()
	if h.githubWebhookHandler == nil {
		t.Fatalf("PostWebhookEvent: no webhook handler wired; call ApplyGitHubChannel or WireGitHubCredentials first")
	}
	path := channelevents.WebhookPathFor(ch.Spec.Kind, ch.Namespace, ch.Name)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signature)
	req.SetPathValue("kind", ch.Spec.Kind)
	req.SetPathValue("ns", ch.Namespace)
	req.SetPathValue("channel", ch.Name)
	rec := httptest.NewRecorder()
	h.githubWebhookHandler.ServeHTTP(rec, req)
	return rec.Result()
}

// sessionsForChannelKey lists every AgentSession in the harness namespace
// carrying the label hash for channelKey — the same lookup
// pkg/channels/channelsd/pipeline.Deliver uses to decide "session already
// exists" vs. "spawn a new one".
func (h *Harness) sessionsForChannelKey(channelKey string) ([]spiceboxv1alpha1.AgentSession, error) {
	var list spiceboxv1alpha1.AgentSessionList
	if err := h.K8s.List(context.Background(), &list,
		// The harness's OWN namespace, not this file's fixture constant: a
		// scenario that boots elsewhere would otherwise list an empty namespace
		// forever and fail as "no session" while the session existed.
		client.InNamespace(h.Namespace()),
		client.MatchingLabels{spiceboxv1alpha1.LabelChannelKey: channelkey.LabelValue(channelKey)},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// EventuallySessions polls until exactly n AgentSessions carry channelKey's
// label hash, or fails the test after timeout.
func (h *Harness) EventuallySessions(t *testing.T, channelKey string, n int, timeout time.Duration) []spiceboxv1alpha1.AgentSession {
	t.Helper()
	var out []spiceboxv1alpha1.AgentSession
	Eventually(t, timeout, func() bool {
		items, err := h.sessionsForChannelKey(channelKey)
		if err != nil || len(items) != n {
			return false
		}
		out = items
		return true
	}, fmt.Sprintf("expected exactly %d AgentSession(s) for channelKey %q", n, channelKey))
	return out
}

// ConsistentlySessions polls for the FULL hold duration, failing immediately
// the moment the session count for channelKey stops being n. Used to prove a
// redelivery produced NO second session, not merely that it hadn't yet at one
// sampled instant.
func (h *Harness) ConsistentlySessions(t *testing.T, channelKey string, n int, hold time.Duration) {
	t.Helper()
	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		items, err := h.sessionsForChannelKey(channelKey)
		if err != nil {
			t.Fatalf("ConsistentlySessions: list: %v", err)
		}
		if len(items) != n {
			t.Fatalf("ConsistentlySessions: channelKey %q now has %d session(s), want it to stay %d", channelKey, len(items), n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// SlackPosts returns everything the harness's shared fakeslack.Client has been
// asked to post into channelID, without waiting.
//
// The fake is an INDEPENDENT record of what was actually posted, not anything
// the logic under test maintains, which is what makes it usable as an assertion
// target at all. Exported and channel-scoped because a fixture chooses its own
// Slack channel id: the dedup scenarios share one constant, a bundle names its
// own in the Channel CR it ships.
func (h *Harness) SlackPosts(channelID string) []fakeslack.Message {
	return h.slackFake.Messages(channelID)
}

// EventuallySlackPosts polls until exactly n messages have landed in the
// reviewbot fixture's Slack output channel, or fails after timeout.
func (h *Harness) EventuallySlackPosts(t *testing.T, n int, timeout time.Duration) []fakeslack.Message {
	t.Helper()
	return h.EventuallySlackPostsIn(t, reviewbotSlackChannelID, n, timeout)
}

// EventuallySlackPostsIn is EventuallySlackPosts against a named channel id.
//
// On timeout it prints what DID land, first line of each. A bare "expected 3,
// timed out" leaves the reader unable to tell "one post is still in flight"
// from "four posted and one is a duplicate" — opposite diagnoses, and the count
// alone names neither.
func (h *Harness) EventuallySlackPostsIn(t *testing.T, channelID string, n int, timeout time.Duration) []fakeslack.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var msgs []fakeslack.Message
	for {
		msgs = h.SlackPosts(channelID)
		if len(msgs) == n {
			return msgs
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "expected exactly %d Slack post(s) in %s after %s; got %d:\n",
		n, channelID, timeout, len(msgs))
	for i, m := range msgs {
		fmt.Fprintf(&b, "  [%d] ts=%s thread=%s %s\n", i, m.TS, m.ThreadTS, firstLine(m.Text))
	}
	t.Fatalf("%s", b.String())
	return nil
}

// firstLine renders one Slack post compactly for a failure message: its first
// line, truncated. Posts here are multi-paragraph reviews, and a full dump of
// several buries the count that is the actual finding.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 90 {
		return line[:90] + "…"
	}
	if line == "" {
		return "(no text)"
	}
	return line
}

// ConsistentlySlackPosts polls fakeslack for the FULL hold duration, failing
// immediately if the post count in the fixture's Slack output channel ever
// differs from n. This is the assertion that actually proves dedup: a
// redelivery that produced a second post fails here even if a narrower,
// one-shot check happened to sample before the second post landed.
func (h *Harness) ConsistentlySlackPosts(t *testing.T, n int, hold time.Duration) {
	t.Helper()
	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		msgs := h.SlackPosts(reviewbotSlackChannelID)
		if len(msgs) != n {
			t.Fatalf("ConsistentlySlackPosts: %s now has %d post(s), want it to stay %d", reviewbotSlackChannelID, len(msgs), n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// SlackThreadTS returns the thread root ts the i'th post (0-indexed, post
// order) belongs to: its own ts if it's a top-level post, its ThreadTS if
// it's a threaded reply. Two posts sharing this value are in the same
// thread.
func (h *Harness) SlackThreadTS(t *testing.T, i int) string {
	t.Helper()
	return h.SlackThreadTSIn(t, reviewbotSlackChannelID, i)
}

// SlackThreadTSIn is SlackThreadTS against a named channel id.
func (h *Harness) SlackThreadTSIn(t *testing.T, channelID string, i int) string {
	t.Helper()
	msgs := h.SlackPosts(channelID)
	if i < 0 || i >= len(msgs) {
		t.Fatalf("SlackThreadTS: index %d out of range (only %d post(s) in %s)", i, len(msgs), channelID)
	}
	m := msgs[i]
	if m.ThreadTS != "" {
		return m.ThreadTS
	}
	return m.TS
}
