package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"golang.org/x/sync/singleflight"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// KindName is the spec.kind value that selects this relsync kind — the same
// string channelkinds.Kind.Name() returns ("github" in kind.go): both name
// the connector a CR's spec.kind selects, not two different things that
// happen to share a string (mirrors slack/relsync_kind.go's own note).
const KindName = "github"

// The three ResourceTypes this kind enumerates, one github_org per
// configured org and github_team/github_repo scopes discovered underneath
// it: github_org is keyed by org login, github_team and github_repo by
// their forge numeric ids.
const (
	githubOrgResourceType  = "github_org"
	githubTeamResourceType = "github_team"
	githubRepoResourceType = "github_repo"
	// githubRepoURLResourceType is the URL-keyed bridge type FetchScope ties
	// every github_repo scope to (see the bridge-edge doc on
	// fetchRepoScope). Not a scope of its own — see toolkits/gh.yaml.
	githubRepoURLResourceType = "github_repo_url"
	// githubRepoURLRepoRelation is the bridge relation itself. Named because
	// it is now read as well as written: ScopeLabelBridges points the console
	// at the same relation fetchRepoScope mints, and the two spellings must
	// not be free to drift apart.
	githubRepoURLRepoRelation = "repo"
	// githubUserResourceType is the identity-join type the useridentity
	// reconciler owns (pkg/authz/spicedb/schema/schema.zed). This kind never
	// writes ITS tuples, only references it as a subject-relation on the
	// roles below.
	githubUserResourceType = "github_user"
)

// githubUserSoleUserRelation is the ONLY github_user subject-relation this
// kind's role writes may use — never "user". "user" is reserved for
// agentsession membership by an invariant test
// (pkg/authz/guardian/schema/github_user_inert_test.go) precisely because a
// second platform subject claiming one GitHub account is tolerated there
// (session standing, revocable) but must not be tolerated here (durable
// repository authority) — see schema.zed's github_user doc.
const githubUserSoleUserRelation = "sole_user"

// The two upstream list endpoints ListScopes walks per configured org, in
// this order — org scopes themselves need no endpoint at all, since the org
// list IS the manifest (see ListScopes's own doc).
const (
	ghPhaseTeams = "teams"
	ghPhaseRepos = "repos"
)

// githubListPageSize bounds one page of GET /orgs/{org}/teams or
// GET /orgs/{org}/repos. GitHub's documented max is 100.
const githubListPageSize = 100

// SyncKind is the relsync.Kind for GitHub's org/team/repo directory: for
// each org in githubConfig.Orgs it enumerates the org itself, its teams
// (GET /orgs/{org}/teams) and its repositories (GET /orgs/{org}/repos), and
// FetchScope turns each into the tuples DirectorySyncSource
// (directory_sync_source.go) claims. Registered under KindName ("github").
//
// # Config, not a field on this type
//
// Orgs to sync arrive on relsync.SourceParams.Config (spec.config), not on
// SyncKind itself: this type is a process-wide registered singleton
// (init() below), and a per-install value stored here would be shared
// across every RelationshipSource CR naming "github" — a defect this repo
// has already shipped and fixed once (CLAUDE.md's SourceParams note).
// parseGithubConfig is the one place Config is read; both ListScopes and
// FetchScope call it fresh, every call.
type SyncKind struct {
	// HTTPClient defaults to http.DefaultClient when nil.
	//
	// Deliberately NOT pkg/x/safehttp.Client(). That guarded dialer refuses
	// any IsPrivate() or loopback destination, which is exactly what a
	// GitHub Enterprise Server install legitimately is
	// (RelationshipSourceSpec.BaseURL's own doc says so). The destination is
	// guarded instead by the CONTROLLER, which runs credhost.Check before the
	// endpoint ever reaches this kind (this type only ever sees
	// relsync.SourceParams{Token, Endpoint, Config}, never the
	// AgentIdentity/credential it was resolved from).
	HTTPClient *http.Client

	// repoCache memoizes owner/name/visibility by credential fingerprint
	// plus forge repo id across ListScopes and FetchScope calls sharing
	// this singleton — see repoByIDCache's own doc for why FetchScope needs
	// it, why the key must include the credential (this singleton is
	// shared across every RelationshipSource CR naming "github"), and why a
	// same-pass, same-credential cache miss cannot happen through
	// relsync.Pass.
	repoCache repoByIDCache

	// teamGrants memoizes which teams grant access to which repository, keyed
	// by credential fingerprint plus forge repo id, built lazily per org from
	// the org-side team endpoints — see repoTeamGrantsCache's own doc for why
	// this replaced the repository-side GET /repos/{owner}/{repo}/teams, why
	// the key must include the credential, and why the edge it feeds still
	// lands on the repo scope's own object.
	teamGrants repoTeamGrantsCache
	// teamGrantsSF coalesces concurrent lazy builds of teamGrants for the same
	// (credential, org): two RelationshipSource CRs sharing this singleton must
	// not each enumerate the same org's teams. Keyed identically to
	// teamGrants.builtOrgs.
	teamGrantsSF singleflight.Group
}

func init() { relsync.Register(&SyncKind{}) }

// Name is the spec.kind value that selects this kind.
func (k *SyncKind) Name() string { return KindName }

// Source returns the already-registered DirectorySyncSource
// (directory_sync_source.go).
func (k *SyncKind) Source() relsource.Source { return DirectorySyncSource }

var _ relsync.ScopeLabeler = (*SyncKind)(nil)

// ScopeLabelBridges points a reader at the URL bridge this kind ALREADY
// writes, so a console row can say `demo-org/widgets` instead of
// `github_repo:1005857813` — see relsync.ScopeLabeler.
//
// Only github_repo has one, and the other two definitions' absence here is
// the honest answer rather than an oversight:
//
//   - github_org is keyed by the org login (canonicalOrgLogin), which IS the
//     name. There is nothing to resolve.
//   - github_team is keyed by the forge's numeric team id and is exactly as
//     unreadable as a repo id was — but this kind stores no team slug
//     anywhere, so there is no bridge to point at. Naming teams needs a
//     stored name, which is a schema change and deliberately not one this
//     makes.
//
// The bridge is fetchRepoScope's own github_repo_url#repo tuple, whose
// resource id is repoURLObjectID's base64url of the canonical
// "https://github.com/<owner>/<name>" — which is why the decoder is
// DecoderB64URL and why the resolved title is a path and the href is that URL.
// This declares where to READ; it neither writes a tuple nor widens what
// DirectorySyncSource claims.
func (k *SyncKind) ScopeLabelBridges() []spicedb.ScopeLabelBridge {
	return []spicedb.ScopeLabelBridge{{
		ScopeDefinition:  githubRepoResourceType,
		BridgeDefinition: githubRepoURLResourceType,
		BridgeRelation:   githubRepoURLRepoRelation,
		Decoder:          resourcedisplay.DecoderB64URL,
	}}
}

// githubConfig is spec.config for this kind.
type githubConfig struct {
	// Orgs is the explicit set of organizations to sync. Required and
	// non-empty: the synced set is defined by the manifest, so a token that
	// gains visibility into a new org does not silently start writing tuples
	// for it.
	Orgs []string `json:"orgs"`
}

// errNoOrgsConfigured is parseGithubConfig's error for a nil, empty or
// org-less spec.config — a named error rather than a silent default, since
// there is no safe default org list: enumerating nothing is indistinguishable
// from a misconfigured CR, and enumerating "whatever the token can see" would
// let a credential's reach — not the manifest — define what gets synced.
var errNoOrgsConfigured = errors.New(`github: spec.config must set a non-empty "orgs" list — the synced set is defined by the manifest, not by a credential's reach`)

// parseGithubConfig parses and validates raw as githubConfig. Called by both
// ListScopes and FetchScope, so a bad or missing config fails the same way
// regardless of which one a caller happens to reach first. Malformed JSON is
// an error, never a default.
func parseGithubConfig(raw json.RawMessage) (githubConfig, error) {
	if len(raw) == 0 {
		return githubConfig{}, errNoOrgsConfigured
	}
	var cfg githubConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return githubConfig{}, fmt.Errorf("github: spec.config is not valid JSON: %w", err)
	}
	if len(cfg.Orgs) == 0 {
		return githubConfig{}, errNoOrgsConfigured
	}
	return cfg, nil
}

// ghCursorState is this kind's own bookkeeping, JSON-encoded into
// relsync.Cursor.Token. It is NOT GitHub's own pagination token — that rides
// inside URL, carried verbatim from a Link header's rel="next" entry and
// never parsed into a page number — it is the position ListScopes' internal
// walk (org index, which of the two upstream lists) needs to know what a
// resumed call should fetch next and how to decode what comes back. Encoded
// fresh on every page that returns a non-zero Next; never persisted beyond
// one relsync.Pass (Cursor's own doc: upstream cursors expire).
type ghCursorState struct {
	OrgIndex int    `json:"orgIndex"`
	Phase    string `json:"phase"`
	URL      string `json:"url"`
}

// decodeGHCursorToken parses a relsync.Cursor.Token already confirmed to
// belong to this kind (via Cursor.ForKind) back into ghCursorState. An empty
// token (the zero Cursor, or ForKind's "start at the beginning" case) means
// fresh: org index 0, about to walk the first configured org's teams.
func decodeGHCursorToken(token string) (ghCursorState, error) {
	if token == "" {
		return ghCursorState{Phase: ghPhaseTeams}, nil
	}
	var s ghCursorState
	if err := json.Unmarshal([]byte(token), &s); err != nil {
		return ghCursorState{}, fmt.Errorf("github: cursor token %q is not a valid enumeration state: %w", token, err)
	}
	// A cursor round-trips through the CR's own status, so it is input, not
	// a value this package can assume it wrote: a negative orgIndex passes
	// ListScopes' `< len(cfg.Orgs)` loop condition and panics on the index
	// that follows, which takes the whole operator down rather than failing
	// this one source. The upper bound needs no check here — it IS the loop
	// condition, and it depends on a config this function does not have.
	if s.OrgIndex < 0 {
		return ghCursorState{}, fmt.Errorf("github: cursor token %q has a negative orgIndex (%d)", token, s.OrgIndex)
	}
	return s, nil
}

// encodeGHCursorToken serializes s for relsync.Cursor.Token. Never errors:
// every field is a plain string or int.
func encodeGHCursorToken(s ghCursorState) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ghTeam is the subset of GitHub's Team object (GET /orgs/{org}/teams) this
// kind reads — just the forge numeric id that keys github_team.
type ghTeam struct {
	ID int64 `json:"id"`
}

// ghRepoOwner is the subset of GitHub's nested owner object this kind reads
// off a Repository — just the login, which for an org-owned repository (the
// only kind ListScopes ever enumerates, via GET /orgs/{org}/repos) IS the
// org's own login.
type ghRepoOwner struct {
	Login string `json:"login"`
}

// ghRepo is the subset of GitHub's Repository object (GET /orgs/{org}/repos)
// this kind reads: the forge numeric id that keys github_repo, plus the
// owner/name/visibility fetchRepoScope needs and GitHub has no by-id lookup
// for — see repoByIDCache's own doc for why this page's own decode is where
// that gets captured, not a later re-fetch.
type ghRepo struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Visibility string      `json:"visibility"`
	Owner      ghRepoOwner `json:"owner"`
}

// repoByIDCache memoizes the (owner, name, visibility) triple for every
// repository ListScopes has decoded in the CURRENT process, keyed by a
// credential fingerprint (see tokenFingerprint) PLUS the forge numeric id —
// github_repo's own key, and the key a github_repo relsync.Scope carries.
//
// It exists because GitHub's REST API has no "get a repository by numeric
// id" route: every collaborator and team-access endpoint
// (GET /repos/{owner}/{name}/collaborators, GET /repos/{owner}/{name}/teams)
// is owner/name-keyed, while github_repo is deliberately keyed by the
// numeric id instead — the half a rename or transfer cannot invalidate (see
// the design doc's "Two keys, one repository"). ListScopes already decodes
// the full repository object per page to mint scopes
// (SyncKind.decodeGHPage's repos branch); this records the owner/name/
// visibility it already has in hand rather than inventing a second fetch
// for information already read once.
//
// The fingerprint half is NOT optional. *SyncKind is a process-wide
// registered singleton shared across every RelationshipSource CR naming
// "github" — a CR pointed at github.com and a CR pointed at a GitHub
// Enterprise Server install (or two different GHES hosts) each have their
// OWN independent repo-id space, so the SAME numeric id can legitimately
// name two completely unrelated repositories. A repo-id-only key let
// whichever CR's ListScopes ran last silently overwrite the other's entry,
// so a scope's FetchScope could resolve owner/name/visibility belonging to
// a DIFFERENT install entirely — writing that install's authorization data
// (a wrong #org edge, a bridge tuple pointing at the wrong URL) into this
// one's graph. Caught in review before merge; see
// TestKind_RepoCacheIsScopedPerCredentialNotJustRepoID. Mirrors
// onepassword's tokenFingerprint-scoped userCache
// (pkg/channels/channelkinds/onepassword/relsync_kind.go) and Slack's own
// slackUserInfoCache in KEY SHAPE only — NOT in eviction policy. Those two
// caches are hit-rate optimizations with a fallback on a miss (re-resolve
// the one member), so a blanket reset under a size cap costs at most one
// extra call. This cache has no such fallback: a miss here is a hard error
// (see fetchRepoScope), so evicting ANY entry mid-ListScopes would fail
// every github_repo scope whose entry it dropped, for the rest of that
// pass — and because a reset clears every fingerprint, one CR's oversized
// org could silently break every OTHER CR sharing this singleton, and the
// pass that just failed would refill the cache and hit the same cap again
// next time, self-perpetuating. So there is no size cap: growth is bounded
// instead by clearForCredential, called once per cycle from ListScopes'
// own token=="" branch, which drops exactly the entries THIS credential
// cached in a PRIOR cycle — bounding this credential's footprint to
// "however many repos it currently has" without ever touching another
// credential's entries.
//
// relsync.Pass's enumerate() always calls ListScopes BEFORE any FetchScope
// it triggers, unconditionally, and derives the scopes it fetches from that
// same call's output (sync.go's Pass) — so in the one real caller, a cache
// miss here cannot happen for a scope Pass actually asks this kind to
// fetch under the SAME credential, because that scope was necessarily JUST
// enumerated by this same long-lived *SyncKind singleton. A miss can only
// happen when something calls FetchScope directly without a preceding
// ListScopes under that credential in this process (a test, or a future
// caller) — see fetchRepoScope's own handling of that case, which fails
// loudly rather than guessing.
type repoByIDCache struct {
	mu   sync.Mutex
	byID map[string]ghRepo
}

func repoByIDCacheKey(fingerprint string, id int64) string {
	return fingerprint + ":" + strconv.FormatInt(id, 10)
}

func (c *repoByIDCache) get(fingerprint string, id int64) (ghRepo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byID[repoByIDCacheKey(fingerprint, id)]
	return r, ok
}

func (c *repoByIDCache) put(fingerprint string, id int64, r ghRepo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string]ghRepo{}
	}
	c.byID[repoByIDCacheKey(fingerprint, id)] = r
}

// clearForCredential drops every entry cached under fingerprint, leaving
// every OTHER credential's entries in this shared singleton untouched — see
// repoByIDCache's own doc on why growth is bounded this way instead of a
// size cap. Called once per ListScopes cycle (its own token=="" branch),
// never mid-cycle, so a repository this credential can no longer see does
// not linger forever, and a cycle in progress never has its own
// not-yet-consumed entries clobbered by itself.
func (c *repoByIDCache) clearForCredential(fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := fingerprint + ":"
	for key := range c.byID {
		if strings.HasPrefix(key, prefix) {
			delete(c.byID, key)
		}
	}
}

// tokenFingerprint derives a cache-scoping key from a credential's raw
// token bytes without ever storing or comparing the token itself — the
// same helper (and the same reasoning) as onepassword's and Slack's own
// tokenFingerprint. Duplicated rather than shared: three packages already
// carry this identical three-line function, matching this repo's own
// precedent of a small per-kind helper over a speculative pkg/x extraction
// (CLAUDE.md's "prefer a library... where the rule stops").
func tokenFingerprint(tok []byte) string {
	sum := sha256.Sum256(tok)
	return hex.EncodeToString(sum[:])
}

// teamRepoGrant is one team's role on one repository, as read from the
// org-side GET /orgs/{org}/teams/{team_slug}/repos endpoint.
type teamRepoGrant struct {
	teamID   int64
	roleName string
}

// repoTeamGrantsCache memoizes, per credential fingerprint, which teams grant
// access to which repository and at what role — the data fetchRepoScope needs
// to write a repo's github_team edges.
//
// It exists for the same reason repoByIDCache does: *SyncKind is a
// process-wide singleton shared across every RelationshipSource CR naming
// "github", and forge repo ids are unique only WITHIN one install, so every
// entry is keyed by a credential fingerprint (see tokenFingerprint) plus the
// forge repo id. It differs from repoByIDCache in WHEN it is populated —
// lazily, on the first fetchRepoScope for an org, not during ListScopes —
// because the data comes from a per-TEAM endpoint
// (GET /orgs/{org}/teams/{team_slug}/repos) ListScopes never walks. The whole
// org is built at once and cached, so an org with T teams costs T+1 calls per
// cycle rather than one call per repo.
//
// # Why org-side, not repo-side
//
// The prior implementation read a repository's team access from the
// repository-side GET /repos/{owner}/{repo}/teams, which the fine-grained PAT
// API gates on "Administration: read" — a permission this sync's setup flow
// never requests (githubSetupScopes asks for Members: read + Metadata: read),
// so every repo's teams call returned 403 and no github_team edge was ever
// written for a fine-grained token. The org-side endpoints need only
// "Members: read", so the read-only token the setup flow provisions can
// produce these edges.
//
// # Why the edge still lands on the repo scope's own object
//
// fetchRepoScope remains the writer: it reads this cache and emits
// github_repo:<id>#<role>@github_team:<tid>#member on its OWN object, so the
// per-scope prune is unchanged. Writing it from a github_team fetch instead
// would land a tuple on a DIFFERENT scope's object (github_repo), which that
// scope's own prune would then delete — the exact cross-resource trap
// fetchTeamScope's subteam-edge doc calls out.
type repoTeamGrantsCache struct {
	mu        sync.Mutex
	byRepo    map[string][]teamRepoGrant // key: fingerprint + ":" + repoID
	builtOrgs map[string]struct{}        // key: fingerprint + ":" + canonicalOrgLogin(org)
}

func repoTeamGrantsKey(fingerprint, repoID string) string { return fingerprint + ":" + repoID }
func builtOrgKey(fingerprint, org string) string          { return fingerprint + ":" + org }

func (c *repoTeamGrantsCache) isBuilt(fingerprint, org string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.builtOrgs[builtOrgKey(fingerprint, org)]
	return ok
}

// store OVERWRITES (never appends to) the grants for every repo the just-
// completed build observed, then marks org built. Overwrite so a repeated
// build for the same org converges to the same set rather than doubling every
// grant; the singleflight guarding ensureTeamGrants already makes concurrent
// first-builds rare, and overwrite makes even those correct. A repo the build
// did not observe keeps no entry, which grantsFor reads as "no team grants" —
// distinct from "org not built yet" only because builtOrgs records the latter.
func (c *repoTeamGrantsCache) store(fingerprint, org string, byRepo map[string][]teamRepoGrant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byRepo == nil {
		c.byRepo = map[string][]teamRepoGrant{}
	}
	for repoID, grants := range byRepo {
		c.byRepo[repoTeamGrantsKey(fingerprint, repoID)] = grants
	}
	if c.builtOrgs == nil {
		c.builtOrgs = map[string]struct{}{}
	}
	c.builtOrgs[builtOrgKey(fingerprint, org)] = struct{}{}
}

func (c *repoTeamGrantsCache) grantsFor(fingerprint, repoID string) []teamRepoGrant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byRepo[repoTeamGrantsKey(fingerprint, repoID)]
}

// clearForCredential drops every entry AND built-org marker cached under
// fingerprint, leaving other credentials' entries untouched — the same
// per-cycle bounding repoByIDCache uses, called from the same place
// (ListScopes' token=="" branch), so a repository or team this credential can
// no longer see does not linger across cycles.
func (c *repoTeamGrantsCache) clearForCredential(fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := fingerprint + ":"
	for k := range c.byRepo {
		if strings.HasPrefix(k, prefix) {
			delete(c.byRepo, k)
		}
	}
	for k := range c.builtOrgs {
		if strings.HasPrefix(k, prefix) {
			delete(c.builtOrgs, k)
		}
	}
}

// ensureTeamGrants populates k.teamGrants for org — once per credential per
// cycle — from the org-side team endpoints, coalescing concurrent builds for
// the same (credential, org) via singleflight so two RelationshipSource CRs
// sharing this singleton do not each enumerate the same org's teams. The build
// error is returned unchanged so fetchRepoScope can fail that repo's scope
// (non-fatal to the pass, recorded in PassResult.ScopeErrors) rather than
// silently writing no team edges.
func (k *SyncKind) ensureTeamGrants(ctx context.Context, params relsync.SourceParams, fingerprint, org string) error {
	if k.teamGrants.isBuilt(fingerprint, org) {
		return nil
	}
	_, err, _ := k.teamGrantsSF.Do(builtOrgKey(fingerprint, org), func() (any, error) {
		// Re-check under the flight: a build that completed while this call
		// waited for the leader has nothing left to do.
		if k.teamGrants.isBuilt(fingerprint, org) {
			return nil, nil
		}
		byRepo, err := k.buildTeamGrants(ctx, params, org)
		if err != nil {
			return nil, err
		}
		k.teamGrants.store(fingerprint, org, byRepo)
		return nil, nil
	})
	return err
}

// buildTeamGrants enumerates org's teams and each team's repositories,
// returning repoID -> the teams that grant it access and at what role. Every
// list is paged to completion (fetchAllGH) before returning: a partial map
// would under-report a repo's team edges, and fetchRepoScope's atomicity
// contract (a complete ScopeContent or an error, never a partial one) forbids
// that. A failure anywhere aborts the whole build rather than caching a map
// missing one team's grants.
func (k *SyncKind) buildTeamGrants(ctx context.Context, params relsync.SourceParams, org string) (map[string][]teamRepoGrant, error) {
	teams, err := fetchAllGH[ghTeamListItem](ctx, k, params, fmt.Sprintf("/orgs/%s/teams?per_page=%d", url.PathEscape(org), githubListPageSize))
	if err != nil {
		return nil, fmt.Errorf("github: org %s teams: %w", org, err)
	}
	byRepo := map[string][]teamRepoGrant{}
	for _, t := range teams {
		repos, err := fetchAllGH[ghTeamRepo](ctx, k, params, fmt.Sprintf("/orgs/%s/teams/%s/repos?per_page=%d",
			url.PathEscape(org), url.PathEscape(t.Slug), githubListPageSize))
		if err != nil {
			return nil, fmt.Errorf("github: org %s team %s repos: %w", org, t.Slug, err)
		}
		for _, r := range repos {
			repoID := strconv.FormatInt(r.ID, 10)
			byRepo[repoID] = append(byRepo[repoID], teamRepoGrant{teamID: t.ID, roleName: r.RoleName})
		}
	}
	return byRepo, nil
}

// freshPathFor builds the first-page request path for phase against org —
// used only when ghCursorState.URL is empty, i.e. this (org, phase) pair
// hasn't been fetched yet this call. Every subsequent page of the SAME
// (org, phase) rides the opaque URL a prior response's Link header handed
// back instead (doGet takes that verbatim), never this path rebuilt with a
// guessed page number.
func freshPathFor(phase, org string) string {
	switch phase {
	case ghPhaseTeams:
		return fmt.Sprintf("/orgs/%s/teams?per_page=%d", url.PathEscape(org), githubListPageSize)
	case ghPhaseRepos:
		return fmt.Sprintf("/orgs/%s/repos?per_page=%d", url.PathEscape(org), githubListPageSize)
	default:
		return ""
	}
}

// decodeGHPage reads and closes resp's body, decoding it as the list shape
// phase implies (teams or repos), and returns the minted scopes plus the
// next page's opaque URL (empty when this was the last page). A method on
// *SyncKind (rather than a free function) so the repos branch can populate
// k.repoCache from the same decode — see repoByIDCache's own doc.
// tokenFP scopes that cache entry to the credential THIS call enumerated
// with, so a different RelationshipSource CR sharing this singleton can
// never read it back for an unrelated repository reusing the same numeric
// id.
func (k *SyncKind) decodeGHPage(tokenFP string, resp *http.Response, phase string) ([]relsync.Scope, string, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("github: GET %s: unexpected status %d", resp.Request.URL, resp.StatusCode)
	}
	switch phase {
	case ghPhaseTeams:
		var teams []ghTeam
		if err := json.NewDecoder(resp.Body).Decode(&teams); err != nil {
			return nil, "", fmt.Errorf("github: decode teams response: %w", err)
		}
		scopes := make([]relsync.Scope, 0, len(teams))
		for _, t := range teams {
			scopes = append(scopes, relsync.Scope{ID: relsync.ScopeID(strconv.FormatInt(t.ID, 10)), ResourceType: githubTeamResourceType})
		}
		return scopes, nextLink(resp), nil
	case ghPhaseRepos:
		var repos []ghRepo
		if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
			return nil, "", fmt.Errorf("github: decode repos response: %w", err)
		}
		scopes := make([]relsync.Scope, 0, len(repos))
		for _, r := range repos {
			scopes = append(scopes, relsync.Scope{ID: relsync.ScopeID(strconv.FormatInt(r.ID, 10)), ResourceType: githubRepoResourceType})
			// Recorded here, not re-fetched later: FetchScope needs the
			// owner/name/visibility this page already decoded to resolve a
			// github_repo scope's forge id back to the path GitHub's
			// collaborator/team-access endpoints require, and there is no
			// by-id lookup to fall back on — see repoByIDCache's own doc.
			k.repoCache.put(tokenFP, r.ID, r)
		}
		return scopes, nextLink(resp), nil
	default:
		return nil, "", fmt.Errorf("github: unknown enumeration phase %q", phase)
	}
}

// ListScopes enumerates every configured org's own scope, then walks each
// org's teams and repositories to completion, in that order — org scopes
// need no upstream call at all (the org list IS the manifest); team and repo
// scopes come from GET /orgs/{org}/teams and GET /orgs/{org}/repos.
//
// One relsync.Pass calls this repeatedly, feeding back whatever Cursor the
// previous call returned, until Next is zero. Internally, ListScopes keeps
// advancing through (org, phase) pairs on its OWN call — issuing one more
// upstream GET each time the current page's Link header carries no
// "rel=\"next\"" entry — and only returns to the caller once a page DOES
// carry one (more of the current (org, phase) list remains, so callers must
// resume it) or every configured org's both lists are exhausted (Complete).
// This mirrors how Slack and 1Password's kinds return one upstream page per
// call — the difference here is that "one page" can span a phase or org
// boundary within a single invocation, never across one, so a page can never
// stand in for more than what it actually fetched (ScopePage.Complete's own
// contract).
func (k *SyncKind) ListScopes(ctx context.Context, params relsync.SourceParams, after relsync.Cursor) (relsync.ScopePage, error) {
	token, err := after.ForKind(KindName)
	if err != nil {
		return relsync.ScopePage{}, err
	}
	cfg, err := parseGithubConfig(params.Config)
	if err != nil {
		return relsync.ScopePage{}, err
	}
	state, err := decodeGHCursorToken(token)
	if err != nil {
		return relsync.ScopePage{}, err
	}
	// Scopes k.repoCache to the credential THIS call is enumerating with —
	// see repoByIDCache's own doc on why the singleton-shared cache must
	// never let one RelationshipSource CR read another's entry for the
	// same numeric repo id.
	tokenFP := tokenFingerprint(params.Token.UnderlyingValue())

	var scopes []relsync.Scope
	if token == "" {
		// Fresh cycle: drop THIS credential's own stale repoCache entries
		// from a prior cycle — a repository it can no longer see must not
		// linger — without touching any other credential sharing this
		// singleton. See repoByIDCache's own doc on why this replaces a
		// size cap.
		k.repoCache.clearForCredential(tokenFP)
		// The team-grants cache is bounded the same way and cleared at the same
		// point — a team's repository access this credential can no longer see
		// must not linger across cycles. See repoTeamGrantsCache's own doc.
		k.teamGrants.clearForCredential(tokenFP)
		// Mint every configured org's own scope up front. No pagination of
		// its own — the org list is the manifest, not an upstream page, so
		// there is nothing to resume here. Canonicalized (see
		// canonicalOrgLogin's own doc) so this scope's own resource id
		// matches the SAME org referenced as a #org SUBJECT from a team or
		// repo fetch, regardless of the casing the operator typed.
		for _, org := range cfg.Orgs {
			scopes = append(scopes, relsync.Scope{ID: relsync.ScopeID(canonicalOrgLogin(org)), ResourceType: githubOrgResourceType})
		}
	}

	for state.OrgIndex < len(cfg.Orgs) {
		if state.Phase != ghPhaseTeams && state.Phase != ghPhaseRepos {
			return relsync.ScopePage{}, fmt.Errorf("github: cursor token names unknown phase %q", state.Phase)
		}
		org := cfg.Orgs[state.OrgIndex]
		target := state.URL
		if target == "" {
			target = freshPathFor(state.Phase, org)
		}

		resp, err := k.doGet(ctx, params, target)
		if err != nil {
			return relsync.ScopePage{}, fmt.Errorf("github: GET org %s %s: %w", org, state.Phase, err)
		}
		pageScopes, next, err := k.decodeGHPage(tokenFP, resp, state.Phase)
		if err != nil {
			return relsync.ScopePage{}, fmt.Errorf("github: org %s %s: %w", org, state.Phase, err)
		}
		scopes = append(scopes, pageScopes...)

		if next != "" {
			return relsync.ScopePage{
				Scopes: scopes,
				Next: relsync.Cursor{
					Kind:  KindName,
					Token: encodeGHCursorToken(ghCursorState{OrgIndex: state.OrgIndex, Phase: state.Phase, URL: next}),
				},
				Complete: false,
			}, nil
		}

		// This phase is exhausted for this org: advance to the next unit of
		// work — repos for the same org, or the next org's teams. Always
		// starting a fresh (URL-less) page, since exhaustion means nothing is
		// left to resume for what we just finished.
		if state.Phase == ghPhaseTeams {
			state = ghCursorState{OrgIndex: state.OrgIndex, Phase: ghPhaseRepos}
		} else {
			state = ghCursorState{OrgIndex: state.OrgIndex + 1, Phase: ghPhaseTeams}
		}
	}

	return relsync.ScopePage{Scopes: scopes, Complete: true}, nil
}

// ghOrgMember is the subset of GitHub's "simple user" object
// (GET /orgs/{org}/members) this kind reads — just the numeric account id
// that keys the github_user identity join.
type ghOrgMember struct {
	ID int64 `json:"id"`
}

// ghTeamMember is the subset of GitHub's "simple user" object
// (GET /orgs/{org}/teams/{team_slug}/members) this kind reads.
type ghTeamMember struct {
	ID int64 `json:"id"`
}

// ghCollaborator is the subset of GitHub's Collaborator object
// (GET /repos/{owner}/{name}/collaborators) this kind reads: the account id
// and role_name — GitHub's modern, custom-role-aware vocabulary ("read",
// "triage", "write", "maintain", "admin"). See repoRoleFromRoleName.
type ghCollaborator struct {
	ID       int64  `json:"id"`
	RoleName string `json:"role_name"`
}

// ghTeamListItem is the subset of GitHub's Team object (GET /orgs/{org}/teams)
// the team-grants build reads: the forge numeric id that keys github_team,
// plus the slug the team's own repository-list endpoint
// (GET /orgs/{org}/teams/{team_slug}/repos) is keyed by. Distinct from ghTeam,
// which decodes only the id for scope enumeration — neither read grows a field
// the other does not use.
type ghTeamListItem struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

// ghTeamRepo is the subset of a Repository object as returned by
// GET /orgs/{org}/teams/{team_slug}/repos — the forge numeric id (which keys
// github_repo) plus role_name, the team's role on that repository in GitHub's
// modern, custom-role-aware vocabulary ("read", "triage", "write", "maintain",
// "admin"). It is the SAME field a collaborator carries, so repoRoleFromRoleName
// maps both — unlike the repository-side GET /repos/{owner}/{repo}/teams this
// replaced, which reported the classic `permission` vocabulary AND needed the
// "Administration: read" repository permission this sync does not request. See
// repoTeamGrantsCache's own doc.
type ghTeamRepo struct {
	ID       int64  `json:"id"`
	RoleName string `json:"role_name"`
}

// ghTeamDetail is GitHub's full Team object as returned by the legacy
// GET /teams/{team_id} — the only REST route that resolves a team's CONTEXT
// (its org login, slug, and parent) from the forge numeric id alone, which
// is what github_team's Scope.ID actually is. The modern replacement
// (GET /orgs/{org}/teams/{team_slug}) needs the org+slug this call exists to
// produce in the first place, so it cannot stand in for it here. Documented
// deprecated, but still present and functional as of GitHub's current REST
// docs; unlike repositories, GitHub never removed the id-only route for
// teams.
type ghTeamDetail struct {
	Slug         string `json:"slug"`
	Organization struct {
		Login string `json:"login"`
	} `json:"organization"`
}

// errGHNotFound is decodeGHTeamDetail's signal for a 404 — the team was
// deleted upstream between enumeration and fetch. Distinguished from any
// other non-200 status so fetchTeamScope can translate it to
// relsync.ErrScopeGone specifically, never a generic fetch error.
var errGHNotFound = errors.New("github: 404 not found")

// decodeGHTeamDetail reads and closes resp's body as a ghTeamDetail.
func decodeGHTeamDetail(resp *http.Response) (ghTeamDetail, error) {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ghTeamDetail{}, errGHNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ghTeamDetail{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var d ghTeamDetail
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return ghTeamDetail{}, fmt.Errorf("decode team detail: %w", err)
	}
	return d, nil
}

// repoRoleFromRoleName maps a collaborator's role_name — GitHub's modern,
// custom-role-aware vocabulary ("read", "triage", "write", "maintain",
// "admin"; GET /repos/{owner}/{name}/collaborators) — onto this schema's
// relation name (toolkits/gh.yaml's rawZed github_repo definition). Every
// value but "read" already matches gh.yaml's relation spelling; "read"
// becomes "reader" because "read" is also this schema's PERMISSION name
// (can_read), and a relation cannot share a permission's name.
//
// The error is not a fetch failure. An org that defines CUSTOM repository
// roles gets the custom role's own name back in this field, which is a
// perfectly healthy API answer this schema simply has no relation for — so
// the caller logs and skips that one collaborator (see fetchRepoScope), and
// never fails the scope over it.
func repoRoleFromRoleName(roleName string) (string, error) {
	switch roleName {
	case "read":
		return "reader", nil
	case "triage", "write", "maintain", "admin":
		return roleName, nil
	default:
		return "", fmt.Errorf("unrecognized collaborator role_name %q", roleName)
	}
}

// canonicalOrgLogin folds an org login to the ONE canonical form every
// github_org resource/subject id must use, regardless of which of three
// independent sources supplied the string: the operator's own
// spec.config.orgs (verbatim, unnormalized — ListScopes' fresh-cycle
// branch applies this to mint the org scope's own id), GitHub's own
// ghTeamDetail.Organization.Login (fetchTeamScope's #org edge), or GitHub's
// own ghRepo.Owner.Login (fetchRepoScope's #org edge). GitHub org logins
// are case-insensitive on the wire (case-preserving as displayed, but the
// API and URLs accept any casing) — without this, an operator configuring
// "acme-corp" against an org whose forge login is "Acme-Corp" would
// enumerate members onto github_org:acme-corp while every #org edge from a
// team or repo fetch named github_org:Acme-Corp: two different SpiceDB
// objects, so `org->member` (toolkits/gh.yaml's can_read) resolves to
// NOBODY for every internal repo — fail-closed, and silent, exactly the
// class of bug CLAUDE.md's canonicalize-before-SpiceDB rule exists to
// prevent. Mirrors repoURLObjectID's own lowercasing of owner/name, for
// the identical reason.
func canonicalOrgLogin(login string) string {
	return strings.ToLower(login)
}

// repoURLObjectID mints the github_repo_url bridge object id: base64url
// (unpadded, per spicedb_escape's safe-set reasoning in the design doc) of
// the canonical "https://github.com/<owner>/<name>" form — byte-for-byte
// the same encoding pkg/authz/transforms.go's gitHubRepoURLID produces for
// every OTHER spelling of this repository (a git remote, an scp url,
// `gh --repo owner/name`), so a running agent's own resolved id and this
// sync's bridge subject land on the identical object. That function folds
// many local spellings (including case) first; this kind already has owner
// and name split cleanly from the forge's own API response, so it
// lowercases and encodes directly rather than importing an unexported
// helper across a package boundary.
//
// KNOWN LIMITATION, recorded rather than fixed here: the base is hardcoded
// to "https://github.com/" even when params.Endpoint names a GitHub
// Enterprise Server host, so a GHES repo and a github.com repo sharing the
// same owner/name collide on one github_repo_url object — the same
// cross-install id-space class as repoByIDCache's own bug, just not
// reachable to fix from this side: gitHubOwnerName (pkg/authz/transforms.go),
// which is what a running agent's OWN call-time resolution goes through,
// is github.com-only too, so this function cannot start keying by the real
// host without the agent-side resolution changing to match. Tracked as a
// separate design change, not this task's fix.
func repoURLObjectID(owner, name string) string {
	canonical := "https://github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(name)
	return base64.RawURLEncoding.EncodeToString([]byte(canonical))
}

// soleUserRoleTuple builds a tuple naming a github_user account as a
// #sole_user subject-set — the ONLY subject-relation this kind's role
// writes may use (see githubUserSoleUserRelation's own doc).
func soleUserRoleTuple(resourceType, resourceID, relation string, accountID int64) spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType:    resourceType,
		ResourceID:      resourceID,
		Relation:        relation,
		SubjectType:     githubUserResourceType,
		SubjectID:       strconv.FormatInt(accountID, 10),
		SubjectRelation: githubUserSoleUserRelation,
	}
}

// FetchScope dispatches on s.ResourceType to the one of three fetchers below
// that reads that scope's real content — see the type's own doc for the
// three ResourceTypes ListScopes ever mints. Every fetcher pages every
// upstream list it reads to COMPLETION before returning: FetchScope's own
// atomicity contract (relsync.Kind's doc) means a partial member or
// collaborator list must never be reported, since the per-scope prune
// deletes whatever this call does not report.
func (k *SyncKind) FetchScope(ctx context.Context, params relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	if _, err := parseGithubConfig(params.Config); err != nil {
		return relsync.ScopeContent{}, err
	}
	switch s.ResourceType {
	case githubOrgResourceType:
		return k.fetchOrgScope(ctx, params, s)
	case githubTeamResourceType:
		return k.fetchTeamScope(ctx, params, s)
	case githubRepoResourceType:
		return k.fetchRepoScope(ctx, params, s)
	default:
		return relsync.ScopeContent{}, fmt.Errorf("github: FetchScope: unknown scope resource type %q", s.ResourceType)
	}
}

// fetchOrgScope resolves one org's membership: GET /orgs/{org}/members,
// paged to completion. s.ID is the org's own login — no resolution needed,
// unlike a team or repo scope (see fetchTeamScope/fetchRepoScope).
// Canonicalized again defensively (ListScopes already canonicalizes when
// minting the scope, so this is idempotent in the real caller) so this
// function is correct even called directly, matching canonicalOrgLogin's
// own doc on why every github_org id must agree.
func (k *SyncKind) fetchOrgScope(ctx context.Context, params relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	org := canonicalOrgLogin(string(s.ID))
	members, err := fetchAllGH[ghOrgMember](ctx, k, params, fmt.Sprintf("/orgs/%s/members?per_page=%d", url.PathEscape(org), githubListPageSize))
	if err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("github: org %s members: %w", org, err)
	}
	tuples := make([]spicedb.Tuple, 0, len(members))
	for _, m := range members {
		tuples = append(tuples, soleUserRoleTuple(githubOrgResourceType, org, "member", m.ID))
	}
	// JoinMisses is structurally always zero for this kind: role subjects
	// are written as github_user:<id>#sole_user and never resolved to a
	// platform identity here at all — the useridentity reconciler's
	// attested edges do that resolution later, from a verified credential.
	// GitHub never reporting an email (the reason this sync was set aside
	// originally) stops mattering because this fetch never needs one. See
	// directory_sync_source.go's own doc and the design doc's "The identity
	// join is free".
	return relsync.ScopeContent{Tuples: tuples}, nil
}

// fetchTeamScope resolves one team's direct membership, its org, and a
// #subteam edge to each team nested directly under it.
//
// s.ID is the team's forge numeric id, which GitHub's *modern* team
// endpoints cannot resolve alone (GET /orgs/{org}/teams/{team_slug} needs
// the org+slug this call has to produce). GET /teams/{team_id} (legacy) is
// the one route that takes the id alone and returns the org login and slug
// in one call — see ghTeamDetail's own doc. Everything after it, the child
// listing included, is a modern org+slug route.
//
// Every tuple it returns lands on this scope's OWN object, which is what
// makes the whole fetch diffable by the ordinary per-scope prune.
func (k *SyncKind) fetchTeamScope(ctx context.Context, params relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	teamID := string(s.ID)
	resp, err := k.doGet(ctx, params, "/teams/"+url.PathEscape(teamID))
	if err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("github: GET team %s: %w", teamID, err)
	}
	detail, err := decodeGHTeamDetail(resp)
	if err != nil {
		if errors.Is(err, errGHNotFound) {
			return relsync.ScopeContent{}, relsync.ErrScopeGone
		}
		return relsync.ScopeContent{}, fmt.Errorf("github: team %s: %w", teamID, err)
	}
	// Canonicalized once, used everywhere below: GitHub's own reported
	// login carries whatever casing that account currently has, which must
	// still agree with the org SCOPE's own resource id (canonicalized the
	// same way at ListScopes' mint site) — see canonicalOrgLogin's own doc.
	org := canonicalOrgLogin(detail.Organization.Login)

	members, err := fetchAllGH[ghTeamMember](ctx, k, params, fmt.Sprintf("/orgs/%s/teams/%s/members?per_page=%d",
		url.PathEscape(org), url.PathEscape(detail.Slug), githubListPageSize))
	if err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("github: team %s members: %w", teamID, err)
	}

	tuples := make([]spicedb.Tuple, 0, len(members)+2)
	tuples = append(tuples, spicedb.Tuple{
		ResourceType: githubTeamResourceType,
		ResourceID:   teamID,
		Relation:     "org",
		SubjectType:  githubOrgResourceType,
		SubjectID:    org,
	})
	for _, m := range members {
		tuples = append(tuples, soleUserRoleTuple(githubTeamResourceType, teamID, "direct_member", m.ID))
	}
	// The nesting edge, emitted DOWNWARD from this team to its children, so
	// it lands on this scope's OWN object. The schema expresses recursive
	// membership itself (`permission member = direct_member +
	// subteam->member` in toolkits/gh.yaml), so a direct edge per level —
	// never this team's members flattened onto its parent — is the whole of
	// what "nested teams" means for this sync.
	//
	// Direction is not a style choice. Writing it from the CHILD's fetch
	// (off the parent field this team detail also carries) puts a tuple on
	// the PARENT's object, and the parent is a scope of the same type: its
	// own diff reads every non-relhash relation on its object, finds a
	// #subteam tuple its own fetch never reported, and deletes it. Neither
	// protection applies — the cross-resource union does record the tuple,
	// and Pass then excludes that entry exactly because github_team is a
	// scope type. The parent gaining a member was enough to drop the edge,
	// and an unchanged child's sentinel then short-circuited its
	// re-assertion, so the child's members silently lost everything they
	// inherited. See TestKind_SubteamEdgeIsWrittenFromTheParentsOwnFetch.
	children, err := fetchAllGH[ghTeam](ctx, k, params, fmt.Sprintf("/orgs/%s/teams/%s/teams?per_page=%d",
		url.PathEscape(org), url.PathEscape(detail.Slug), githubListPageSize))
	if err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("github: team %s child teams: %w", teamID, err)
	}
	for _, c := range children {
		tuples = append(tuples, spicedb.Tuple{
			ResourceType: githubTeamResourceType,
			ResourceID:   teamID,
			Relation:     "subteam",
			SubjectType:  githubTeamResourceType,
			SubjectID:    strconv.FormatInt(c.ID, 10),
		})
	}
	return relsync.ScopeContent{Tuples: tuples}, nil
}

// fetchRepoScope resolves one repository's direct collaborators, its
// team-granted access, its org edge (internal visibility only), and the
// github_repo_url bridge edge.
//
// s.ID is the repo's forge numeric id. GitHub's REST API has no "get a
// repository by numeric id" route (unlike teams — see fetchTeamScope), so
// owner/name/visibility come from k.repoCache, populated by ListScopes'
// own decode of GET /orgs/{org}/repos — see repoByIDCache's own doc,
// including why a miss here cannot happen through the real caller
// (relsync.Pass), and why the lookup is scoped to THIS call's credential
// rather than the id alone (the singleton is shared across every
// RelationshipSource CR naming "github", and forge repo ids are unique only
// within one install).
func (k *SyncKind) fetchRepoScope(ctx context.Context, params relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	repoIDStr := string(s.ID)
	repoID, err := strconv.ParseInt(repoIDStr, 10, 64)
	if err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("github: scope id %q is not a forge repository id: %w", repoIDStr, err)
	}
	tokenFP := tokenFingerprint(params.Token.UnderlyingValue())
	detail, ok := k.repoCache.get(tokenFP, repoID)
	if !ok {
		return relsync.ScopeContent{}, fmt.Errorf(
			"github: repo %d: owner/name not on record for this credential — FetchScope needs a prior ListScopes, under the SAME credential, in this process to have enumerated this repository; relsync.Pass guarantees that for every scope it fetches, so seeing this means FetchScope was called directly without one (a test must seed SyncKind.repoCache first)",
			repoID)
	}
	owner, name := detail.Owner.Login, detail.Name

	collaborators, err := fetchAllGH[ghCollaborator](ctx, k, params, fmt.Sprintf("/repos/%s/%s/collaborators?affiliation=direct&per_page=%d",
		url.PathEscape(owner), url.PathEscape(name), githubListPageSize))
	if err != nil {
		if errors.Is(err, errGHNotFound) {
			// The repository was deleted upstream between enumeration and
			// this fetch — distinct from becoming merely invisible to the
			// token (which this sync deliberately does not reap; see
			// repoURLObjectID's own doc on the bridge's singularity). A 404
			// here means the object itself is gone, so this scope is gone
			// too, matching fetchTeamScope's own 404 handling.
			return relsync.ScopeContent{}, relsync.ErrScopeGone
		}
		return relsync.ScopeContent{}, fmt.Errorf("github: repo %s/%s collaborators: %w", owner, name, err)
	}

	var tuples []spicedb.Tuple
	for _, c := range collaborators {
		relation, err := repoRoleFromRoleName(c.RoleName)
		if err != nil {
			// SKIPPED, not fatal, and the difference is much larger than the
			// one grant: an org using CUSTOM repository roles gets that
			// role's own name back here, and failing the scope would mean
			// this repository gets no tuples at all (fail-closed, fine on its
			// own), Ready stays Synced because a scope error is non-fatal
			// (misleading), and fetchCoverage can never again reach
			// len(scopes) — which is the gate the cross-resource bridge reap
			// runs behind. One custom role would disable reaping for the
			// whole source, permanently.
			//
			// Logged rather than swallowed: a role this schema cannot express
			// is a real gap in what got synced, and an operator has to be able
			// to find out which repository and which role it was.
			logr.FromContextOrDiscard(ctx).Info("github: skipping a collaborator whose repository role this schema cannot express; every other grant on this repository still syncs",
				"repo", owner+"/"+name, "repoID", repoIDStr, "collaboratorID", c.ID, "roleName", c.RoleName, "err", err.Error())
			continue
		}
		tuples = append(tuples, soleUserRoleTuple(githubRepoResourceType, repoIDStr, relation, c.ID))
	}
	// Team-granted access, sourced from the org-side team endpoints (see
	// repoTeamGrantsCache's own doc) — NOT the repository-side
	// GET /repos/{owner}/{repo}/teams, which the fine-grained PAT API gates on
	// "Administration: read", a permission this sync never requests. The grants
	// are built once per org per cycle and cached; the edge still lands on this
	// github_repo scope's OWN object, so the per-scope prune is unchanged.
	if err := k.ensureTeamGrants(ctx, params, tokenFP, canonicalOrgLogin(owner)); err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("github: repo %s/%s team access: %w", owner, name, err)
	}
	for _, g := range k.teamGrants.grantsFor(tokenFP, repoIDStr) {
		relation, err := repoRoleFromRoleName(g.roleName)
		if err != nil {
			// SKIPPED, not fatal — the same reasoning as the collaborator branch
			// above: an org with CUSTOM repository roles gets the custom role's
			// own name back here too (this endpoint reports the SAME role_name
			// vocabulary a collaborator does), and failing the scope would drop
			// every other grant on this repository and disable the
			// cross-resource reap for the whole source. Logged so the gap is
			// findable.
			logr.FromContextOrDiscard(ctx).Info("github: skipping a team grant whose repository role this schema cannot express; every other grant on this repository still syncs",
				"repo", owner+"/"+name, "repoID", repoIDStr, "teamID", g.teamID, "roleName", g.roleName, "err", err.Error())
			continue
		}
		tuples = append(tuples, spicedb.Tuple{
			ResourceType:    githubRepoResourceType,
			ResourceID:      repoIDStr,
			Relation:        relation,
			SubjectType:     githubTeamResourceType,
			SubjectID:       strconv.FormatInt(g.teamID, 10),
			SubjectRelation: "member",
		})
	}

	if detail.Visibility == "internal" {
		// Only internal visibility relates a repo to its org — private gets
		// no #org edge, and public gets none either. Public repos are NOT
		// given a wildcard reader instead: pttagmint intersects raw subject
		// ids as opaque strings (minter.go), so a `user:*` arm would arrive
		// as the literal id "*" and match nobody — a public repo's memory
		// would resolve to an audience of no one while every permission
		// check on it still said yes. See toolkits/gh.yaml's own note on
		// `reader` carrying no `user:*` arm.
		tuples = append(tuples, spicedb.Tuple{
			ResourceType: githubRepoResourceType,
			ResourceID:   repoIDStr,
			Relation:     "org",
			SubjectType:  githubOrgResourceType,
			SubjectID:    canonicalOrgLogin(owner),
		})
	}

	// The URL->id bridge. Emitted as part of THIS scope's own ScopeContent,
	// but its resource (github_repo_url) is NOT this scope's own object
	// (github_repo) — a cross-resource tuple, Ruling 4 in this plan's SDD
	// ledger, the same shape as Slack's slack_user#user edge. That is what
	// makes it singular BY CONSTRUCTION rather than by eventual reap: Pass's
	// extraResources union is keyed by the tuple's own Key(), so a stale
	// edge on the SAME URL object naming a DIFFERENT (old) repo id has a
	// different Key(), falls out of the union the moment THIS fetch is the
	// one asserting the URL's current truth, and is deleted by
	// reapAbsentCrossResourceTuples once a pass achieves full fetch
	// coverage. This kind's job ends at "exactly one github_repo_url#repo
	// tuple per repo scope" — what happens to any OTHER edge on that same
	// URL object is the engine's half, not this function's; see this plan's
	// task brief correction and TestKind_URLBridgeIsSingular.
	tuples = append(tuples, spicedb.Tuple{
		ResourceType: githubRepoURLResourceType,
		ResourceID:   repoURLObjectID(owner, name),
		Relation:     githubRepoURLRepoRelation,
		SubjectType:  githubRepoResourceType,
		SubjectID:    repoIDStr,
	})

	return relsync.ScopeContent{Tuples: tuples}, nil
}
