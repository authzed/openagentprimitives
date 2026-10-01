package mcpfront

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/webui/livemirror"
)

// The three $sameperm mirror names this file checks, pinned as constants so a
// typo can't silently check the wrong permission — and so
// TestRoleMatrixOnTools can assert on the exact string a tool uses without
// duplicating the literal everywhere. See accesstoken.MirrorPermissions for
// the full set these are drawn from.
const (
	permReadTranscript = "read_transcript"
	permRead           = "read"
	permView           = "view"
)

// maxReadableSessionRefs is a defensive second cap on the refs this file fans
// K8s Gets out over, mirroring pkg/web/webui/sessions' maxListedSessions.
// d.LookupReadableSessions already bounds its own answer (webd's
// mcpReadableSessionsLimit), so this is belt-and-suspenders against a future
// Deps implementation that doesn't.
const maxReadableSessionRefs = 200

// readableSessionConcurrency bounds the parallel Gets fanned out over the
// lookup's refs, the same discipline pkg/web/webui/sessions/list.go uses
// (listGetConcurrency) for the identical reason: a direct, non-cache-backed
// K8s client turns every row into one apiserver round trip.
const readableSessionConcurrency = 8

// maxSearchDerivedScopes caps the number of session scopes search_memory
// derives when the caller names no explicit session — see opSearchMemory.
const maxSearchDerivedScopes = 20

// maxArtifactContentBytes caps get_artifact's inline content; beyond this the
// content is truncated and GetArtifactOut.ContentTruncated is set.
const maxArtifactContentBytes = 256 * 1024

// defaultListSessionsLimit / maxListSessionsLimit are list_sessions' Limit
// field defaults/cap.
const (
	defaultListSessionsLimit = 50
	maxListSessionsLimit     = 200
)

// defaultTranscriptLimit is get_transcript's Limit field default.
const defaultTranscriptLimit = 100

// errSessionNotAccessible is the ONE denial text authorizeSessionOp ever
// returns, for BOTH "no such session" and "it exists but the three-legged
// check denied" — there is no existence oracle for a bearer that cannot read
// a session to learn whether it is even there. Every call site below returns
// this value verbatim rather than wrapping it, so the bytes a caller sees
// never vary by failure mode.
var errSessionNotAccessible = errors.New("session not found or not accessible")

// ListSessionsIn / SessionSummary / ListSessionsOut, GetSessionIn/Out,
// GetTranscriptIn/TranscriptEntry/Out, SearchMemoryIn/SearchHit/Out,
// ListArtifactsIn/ArtifactSummary/Out and GetArtifactIn/Out are the shared
// typed contract the six /mcp tools expose — also the contract the Phase-2
// CLI backend imports directly (see task-10-brief.md), so field names and
// JSON tags here are the wire format, not an implementation detail.
type ListSessionsIn struct {
	Agent string `json:"agent,omitempty"` // agentclass name filter
	State string `json:"state,omitempty"` // "running"|"ended"|"" for all
	Limit int    `json:"limit,omitempty"` // default 50, cap 200
}

type SessionSummary struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Class     string    `json:"class"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"startedAt"`
	// Channel summarizes spec.inputChannel: kind, key, and the kind-specific
	// External routing metadata, copied verbatim. Omitted for a kubectl-driven
	// session with no input channel.
	Channel map[string]string `json:"channel,omitempty"`
}

type ListSessionsOut struct {
	Sessions []SessionSummary `json:"sessions"`
}

type GetSessionIn struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type GetSessionOut struct {
	Session SessionSummary `json:"session"`
}

type GetTranscriptIn struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Offset    int    `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"` // entries; default 100
}

type TranscriptEntry struct {
	Kind      string    `json:"kind"`
	Role      string    `json:"role,omitempty"`
	Text      string    `json:"text,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type GetTranscriptOut struct {
	Entries []TranscriptEntry `json:"entries"`
	Total   int               `json:"total"`
}

type SearchMemoryIn struct {
	Query string `json:"query"`
	// Namespace+Name together name a single session to search; both empty
	// searches every session this token's owner may read_transcript.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type SearchHit struct {
	Session string  `json:"session"`
	Kind    string  `json:"kind"`
	ID      string  `json:"id"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

type SearchMemoryOut struct {
	Hits []SearchHit `json:"hits"`
	// SearchUnavailable is true when no search provider is configured
	// (memory.ErrNoSearchProviders) — distinct from "no hits", so a caller
	// doesn't read an empty result as "nothing matched".
	SearchUnavailable bool `json:"searchUnavailable,omitempty"`
}

type ListArtifactsIn struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type ArtifactSummary struct {
	ArtifactID   string    `json:"artifactId"`
	Name         string    `json:"name,omitempty"`
	Description  string    `json:"description,omitempty"`
	RendererKind string    `json:"rendererKind"`
	CreatedAt    time.Time `json:"createdAt"`
}

type ListArtifactsOut struct {
	Artifacts []ArtifactSummary `json:"artifacts"`
}

type GetArtifactIn struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	ArtifactID string `json:"artifactId"`
}

type GetArtifactOut struct {
	Artifact ArtifactSummary `json:"artifact"`
	Content  string          `json:"content,omitempty"`
	// ContentTruncated is true when the artifact's content exceeded
	// maxArtifactContentBytes and was cut to fit.
	ContentTruncated bool `json:"contentTruncated,omitempty"`
}

// recordDecision is the ops layer's audit seam: every authorization decision
// an op makes — allowed, or denied naming which leg of the three-way check
// failed — is recorded through this ONE function. Today it only logs; Task
// 10b replaces the body with a write through the signed component-publisher
// path into a durable authzdecision entry (DecisionRecorder), without
// touching any call site below.
func recordDecision(d Deps, act acting, permission, resourceType, resourceID string, allowed bool, failedLeg string) {
	d.Logger().Info("mcpfront: access-token op decision",
		"tokenID", act.TokenID, "owner", act.Owner.String(), "permission", permission,
		"resourceType", resourceType, "resourceID", resourceID, "allowed", allowed, "failedLeg", failedLeg)
}

// failedLegOf names which leg of a denied AccessTokenDecision failed, for
// recordDecision's audit trail. "" for an allowed decision.
func failedLegOf(dec spicedb.AccessTokenDecision) string {
	switch {
	case !dec.TokenGrants:
		return "token"
	case !dec.ScopeCovers:
		return "scope"
	case !dec.OwnerHas:
		return "owner"
	default:
		return ""
	}
}

// sessionResourcePermission maps a token mirror permission to the permission
// leg 3 checks on the agentsession itself (AccessTokenCheck.ResourcePermission).
//
// WHY the "read"/"view" mirrors map to read_transcript: agentsession defines
// no "read" or "view" permission, and the session-level truth each mirror
// stands on IS read_transcript —
//   - memory_entry#read is literally defined as session->read_transcript in
//     the schema, so for a whole-session memory read, read_transcript on the
//     session is the same question asked one hop earlier;
//   - artifact#view adds org-view (parent->artifact_org_view) and platform
//     admin (platform->view_audit) arms on top of parent->interact. Gating a
//     token's artifact reads on session-level read_transcript is deliberately
//     NARROWER: token holders do not get those side doors in v1 — an
//     org-wide artifact audience or an admin's audit standing must not leak
//     through a delegated bearer token.
//
// Every other mirror ("read_transcript", "interact", "approve", ...) is
// literally the session permission of the same name; empty means "same as
// the mirror" (CheckAccessTokenOp's default), keeping this map a closed
// exception list rather than a parallel naming scheme.
func sessionResourcePermission(mirrorPerm string) string {
	switch mirrorPerm {
	case permRead, permView:
		return permReadTranscript
	default:
		return ""
	}
}

// authorizeSessionOp loads the AgentSession CR and runs the three-legged
// CheckAccessTokenOp for perm against it. Denial and not-found BOTH return
// errSessionNotAccessible — verbatim, never wrapped — so a caller cannot tell
// "it doesn't exist" from "it exists but you may not touch it" (no existence
// oracle for a bearer token).
//
// fullyConsistent=true on every call: correctness over latency for a per-call
// authorization gate that denies by default. A future refinement could cache
// a ZedToken per (token id, class) and pass fullyConsistent only on this
// process's first check of it, trading a little staleness tolerance for one
// fewer round trip to the datastore's leader — not implemented here; see
// task-10-brief.md's binding design notes.
func authorizeSessionOp(ctx context.Context, d Deps, act acting, perm, ns, name string) (*spiceboxv1alpha1.AgentSession, error) {
	var sess spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		if !apierrors.IsNotFound(err) {
			d.Logger().Info("mcpfront: authorizeSessionOp Get errored; treating as not accessible",
				"ns", ns, "name", name, "tokenID", act.TokenID, "perm", perm, "err", err.Error())
		}
		recordDecision(d, act, perm, "agentsession", ns+"/"+name, false, "")
		return nil, errSessionNotAccessible
	}

	classID := sess.Namespace + "/" + sess.Spec.Class
	dec, err := d.AccessTokenAuthz().CheckAccessTokenOp(ctx, spicedb.AccessTokenCheck{
		TokenID:            act.TokenID,
		Owner:              act.Owner,
		Permission:         perm,
		ResourceType:       "agentsession",
		ResourceID:         ns + "/" + name,
		ClassID:            classID,
		ResourcePermission: sessionResourcePermission(perm),
	}, true)
	if err != nil {
		d.Logger().Info("mcpfront: authorizeSessionOp check errored; denying",
			"ns", ns, "name", name, "tokenID", act.TokenID, "perm", perm, "err", err.Error())
		recordDecision(d, act, perm, "agentsession", ns+"/"+name, false, "")
		return nil, errSessionNotAccessible
	}
	if !dec.Allowed() {
		recordDecision(d, act, perm, "agentsession", ns+"/"+name, false, failedLegOf(dec))
		return nil, errSessionNotAccessible
	}
	recordDecision(d, act, perm, "agentsession", ns+"/"+name, true, "")
	return &sess, nil
}

// readApprovedContext mints a memory.ForBearerToken capability approval for
// resource and attaches it to ctx, for the narrow case where d.Artifacts()
// reaches a memory.Local in-process (that facade's Query/Get enforce
// memory.EnsureApproval on every call). Called only AFTER authorizeSessionOp
// has already proven the three-legged check for this exact resource — exactly
// the contract ForBearerToken documents ("call only after the token-scope
// check has passed"). In production d.Artifacts() wraps an httpclient.Client
// instead (an HTTP call to the operator, authorized at that boundary), which
// never inspects ctx's approval set, so this is a no-op there and only
// matters for an in-process memory.Memory implementation.
func readApprovedContext(ctx context.Context, act acting, resource string) context.Context {
	return memory.WithApproval(ctx, memory.ForBearerToken(memory.ReadMemory, resource, act.TokenID))
}

// coveredSessionRow is one session this token's owner may read AND whose
// agentclass the token's scope covers — the join list_sessions and
// search_memory's session-less derivation both need.
type coveredSessionRow struct {
	Ref   spicedb.SessionRef
	Sess  *spiceboxv1alpha1.AgentSession
	Class string // ns/class
}

// coveredSessions is the shared derivation behind list_sessions and
// search_memory's no-explicit-session branch: gate on the token's role
// mirror for mirrorPerm (leg 1 ALONE — cheaper than a full CheckAccessTokenOp
// per candidate), then join the owner's readable sessions against the K8s
// objects and filter to the classes the token's scope actually covers (leg
// 2, bulk, once).
//
// A false mirror check returns an EMPTY result with no error and, crucially,
// never calls LookupReadableSessions or touches K8s at all — the brief's
// "no lookup/K8s reads" requirement, so a read-role token asking a
// write-scoped question costs nothing beyond the one mirror check.
func coveredSessions(ctx context.Context, d Deps, act acting, mirrorPerm string) ([]coveredSessionRow, error) {
	mirrorOK, err := d.AccessTokenAuthz().CheckAccessTokenMirror(ctx, act.TokenID, act.Owner, mirrorPerm, false)
	if err != nil {
		d.Logger().Info("mcpfront: coveredSessions mirror check errored; returning empty",
			"tokenID", act.TokenID, "permission", mirrorPerm, "err", err.Error())
		recordDecision(d, act, mirrorPerm, "accesstoken", act.TokenID, false, "token")
		return nil, nil
	}
	if !mirrorOK {
		recordDecision(d, act, mirrorPerm, "accesstoken", act.TokenID, false, "token")
		return nil, nil
	}
	recordDecision(d, act, mirrorPerm, "accesstoken", act.TokenID, true, "")

	lookup, err := d.LookupReadableSessions(ctx, act.Owner)
	if err != nil {
		return nil, fmt.Errorf("mcpfront: lookup readable sessions: %w", err)
	}

	refs := lookup.Refs
	if len(refs) > maxReadableSessionRefs {
		refs = refs[:maxReadableSessionRefs]
	}

	rows := make([]*coveredSessionRow, len(refs))
	var mu sync.Mutex
	classSet := map[string]struct{}{}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(readableSessionConcurrency)
	for i, r := range refs {
		g.Go(func() error {
			var sess spiceboxv1alpha1.AgentSession
			if err := d.K8s().Get(gctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, &sess); err != nil {
				if apierrors.IsNotFound(err) {
					d.Logger().Info("mcpfront: readable session has no Kubernetes object; dropping",
						"ns", r.Namespace, "name", r.Name, "tokenID", act.TokenID)
				} else {
					d.Logger().Info("mcpfront: get AgentSession failed; dropping",
						"ns", r.Namespace, "name", r.Name, "tokenID", act.TokenID, "err", err.Error())
				}
				return nil // never abort the group: one unreadable session must not blank the rest
			}
			// A delegated child is machinery, not a conversation a token holder
			// asked for — same exclusion pkg/web/webui/sessions/list.go applies.
			if sess.Spec.Parent != nil {
				return nil
			}
			classID := sess.Namespace + "/" + sess.Spec.Class
			mu.Lock()
			classSet[classID] = struct{}{}
			mu.Unlock()
			rows[i] = &coveredSessionRow{Ref: r, Sess: &sess, Class: classID}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("mcpfront: fan out session gets: %w", err)
	}

	classIDs := make([]string, 0, len(classSet))
	for c := range classSet {
		classIDs = append(classIDs, c)
	}
	covered, err := d.AccessTokenAuthz().FilterAccessTokenCoveredClasses(ctx, act.TokenID, classIDs, false)
	if err != nil {
		return nil, fmt.Errorf("mcpfront: filter covered classes: %w", err)
	}

	out := make([]coveredSessionRow, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		if !covered[row.Class] {
			continue
		}
		out = append(out, *row)
	}
	return out, nil
}

// opListSessions implements the list_sessions tool.
func opListSessions(ctx context.Context, d Deps, act acting, in ListSessionsIn) (ListSessionsOut, error) {
	rows, err := coveredSessions(ctx, d, act, permReadTranscript)
	if err != nil {
		return ListSessionsOut{}, err
	}

	limit := in.Limit
	if limit <= 0 {
		limit = defaultListSessionsLimit
	}
	if limit > maxListSessionsLimit {
		limit = maxListSessionsLimit
	}

	summaries := make([]SessionSummary, 0, len(rows))
	for _, row := range rows {
		if in.Agent != "" && row.Sess.Spec.Class != in.Agent {
			continue
		}
		if in.State != "" {
			ended := sessionIsEnded(row.Sess.Status.Phase)
			if (in.State == "ended") != ended {
				continue
			}
		}
		summaries = append(summaries, sessionSummaryFrom(row.Sess))
	}

	// Deterministic order: newest first, tiebroken by namespace/name — two
	// identical calls must return the same order (sort.Slice is not stable and
	// StartedAt alone collides at second granularity).
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].StartedAt.Equal(summaries[j].StartedAt) {
			return summaries[i].StartedAt.After(summaries[j].StartedAt)
		}
		if summaries[i].Namespace != summaries[j].Namespace {
			return summaries[i].Namespace < summaries[j].Namespace
		}
		return summaries[i].Name < summaries[j].Name
	})

	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return ListSessionsOut{Sessions: summaries}, nil
}

// sessionIsEnded reports whether phase is one of the two terminal phases —
// the same two-bucket split ListSessionsIn.State offers ("running"|"ended").
func sessionIsEnded(phase string) bool {
	return phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded || phase == spiceboxv1alpha1.AgentSessionPhaseFailed
}

// sessionSummaryFrom maps an AgentSession CR to the wire SessionSummary.
func sessionSummaryFrom(sess *spiceboxv1alpha1.AgentSession) SessionSummary {
	var startedAt time.Time
	switch {
	case sess.Status.StartedAt != nil:
		startedAt = sess.Status.StartedAt.Time.UTC()
	case !sess.CreationTimestamp.IsZero():
		startedAt = sess.CreationTimestamp.Time.UTC()
	}

	var channel map[string]string
	if ic := sess.Spec.InputChannel; ic != nil {
		channel = make(map[string]string, len(ic.External)+2)
		channel["kind"] = ic.Kind
		channel["key"] = ic.Key
		for k, v := range ic.External {
			channel[k] = v
		}
	}

	return SessionSummary{
		Namespace: sess.Namespace,
		Name:      sess.Name,
		Class:     sess.Spec.Class,
		Phase:     sess.Status.Phase,
		StartedAt: startedAt,
		Channel:   channel,
	}
}

// opGetSession implements the get_session tool.
func opGetSession(ctx context.Context, d Deps, act acting, in GetSessionIn) (GetSessionOut, error) {
	sess, err := authorizeSessionOp(ctx, d, act, permReadTranscript, in.Namespace, in.Name)
	if err != nil {
		return GetSessionOut{}, err
	}
	return GetSessionOut{Session: sessionSummaryFrom(sess)}, nil
}

// opGetTranscript implements the get_transcript tool.
func opGetTranscript(ctx context.Context, d Deps, act acting, in GetTranscriptIn) (GetTranscriptOut, error) {
	if _, err := authorizeSessionOp(ctx, d, act, permReadTranscript, in.Namespace, in.Name); err != nil {
		return GetTranscriptOut{}, err
	}

	hist, err := livemirror.ReadHistory(ctx, d.OperatorURL(), d.MemoryToken(), in.Namespace, in.Name, d.Logger())
	if err != nil {
		return GetTranscriptOut{}, fmt.Errorf("mcpfront: read transcript: %w", err)
	}

	total := len(hist.Timeline)
	limit := in.Limit
	if limit <= 0 {
		limit = defaultTranscriptLimit
	}
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}

	var entries []TranscriptEntry
	if offset < total {
		end := offset + limit
		if end > total {
			end = total
		}
		slice := hist.Timeline[offset:end]
		entries = make([]TranscriptEntry, 0, len(slice))
		for _, it := range slice {
			entries = append(entries, TranscriptEntry{
				Kind: it.Kind, Role: it.Role, Text: it.Text, CreatedAt: it.CreatedAt,
			})
		}
	}

	return GetTranscriptOut{Entries: entries, Total: total}, nil
}

// opSearchMemory implements the search_memory tool.
//
// Pre-authorization is mandatory here, not optional hardening: the webd
// memory token this process holds (d.MemoryToken()) can read ANY session's
// memory — it is an operator-wide credential, not scoped per caller. So the
// SCOPE LIST passed to the search request IS the entire authorization
// boundary for this call. Building that list from anything other than
// sessions this token has already been proven to cover (via authorizeSessionOp
// for an explicit session, or coveredSessions' leg-1+leg-2 join otherwise)
// would let a read-scoped token search sessions its owner — or its own
// class scope — was never granted.
func opSearchMemory(ctx context.Context, d Deps, act acting, in SearchMemoryIn) (SearchMemoryOut, error) {
	var scopes []memory.Scope

	if in.Namespace != "" && in.Name != "" {
		if _, err := authorizeSessionOp(ctx, d, act, permRead, in.Namespace, in.Name); err != nil {
			return SearchMemoryOut{}, err
		}
		scopes = []memory.Scope{{Kind: "session", ID: in.Namespace + "/" + in.Name}}
	} else {
		rows, err := coveredSessions(ctx, d, act, permRead)
		if err != nil {
			return SearchMemoryOut{}, err
		}
		if len(rows) > maxSearchDerivedScopes {
			rows = rows[:maxSearchDerivedScopes]
		}
		scopes = make([]memory.Scope, 0, len(rows))
		for _, row := range rows {
			scopes = append(scopes, memory.Scope{Kind: "session", ID: row.Ref.Namespace + "/" + row.Ref.Name})
		}
	}

	if len(scopes) == 0 {
		return SearchMemoryOut{}, nil
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}

	mem := httpclient.New(d.OperatorURL(), d.MemoryToken())
	res, err := mem.Search(ctx, memory.SearchRequest{Scopes: scopes, Text: in.Query, Limit: limit})
	if err != nil {
		if errors.Is(err, memory.ErrNoSearchProviders) {
			return SearchMemoryOut{SearchUnavailable: true}, nil
		}
		return SearchMemoryOut{}, fmt.Errorf("mcpfront: search memory: %w", err)
	}

	hits := make([]SearchHit, 0, len(res.Entries))
	for _, se := range res.Entries {
		hits = append(hits, SearchHit{
			Session: se.Entry.Scope.ID,
			Kind:    se.Entry.Kind,
			ID:      se.Entry.ID,
			Snippet: snippetFrom(se.Entry.Content),
			Score:   se.Score,
		})
	}
	return SearchMemoryOut{Hits: hits}, nil
}

// maxSnippetRunes bounds search_memory's Snippet field — a preview, not the
// full stored content (search_memory the agent tool relays full content;
// this /mcp surface deliberately doesn't, since its caller is a remote client
// browsing results, not a model deciding whether to fetch more).
const maxSnippetRunes = 200

// snippetFrom renders a bounded preview of an entry's raw content for
// SearchHit.Snippet. content is the Kind's opaque ContentSchema payload (JSON
// bytes); this intentionally does not attempt to parse it by Kind — a tool
// generic across every memory Kind can't special-case each one's shape — so
// the preview is simply the content's own JSON text, truncated at a rune
// boundary so truncation can never split a multi-byte character.
func snippetFrom(content []byte) string {
	s := string(content)
	if utf8.RuneCountInString(s) <= maxSnippetRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxSnippetRunes])
}

// opListArtifacts implements the list_artifacts tool.
func opListArtifacts(ctx context.Context, d Deps, act acting, in ListArtifactsIn) (ListArtifactsOut, error) {
	if _, err := authorizeSessionOp(ctx, d, act, permView, in.Namespace, in.Name); err != nil {
		return ListArtifactsOut{}, err
	}

	scope := memory.Scope{Kind: "session", ID: in.Namespace + "/" + in.Name}
	views, err := d.Artifacts().ListArtifacts(readApprovedContext(ctx, act, scope.ID), scope)
	if err != nil {
		return ListArtifactsOut{}, fmt.Errorf("mcpfront: list artifacts: %w", err)
	}

	out := make([]ArtifactSummary, 0, len(views))
	for _, v := range views {
		out = append(out, artifactSummaryFrom(v))
	}
	return ListArtifactsOut{Artifacts: out}, nil
}

// artifactCreatedAtLayout matches artifacts.ArtifactView.CreatedAt's format
// (UTC, "2006-01-02T15:04:05Z") exactly — see pkg/platform/artifacts/resolve.go.
const artifactCreatedAtLayout = "2006-01-02T15:04:05Z"

// artifactSummaryFrom maps an artifacts.ArtifactView to the wire
// ArtifactSummary. An unparsable CreatedAt (should not happen; the service
// always writes this exact layout) degrades to the zero time rather than
// failing the whole list — one malformed timestamp must not blank a page of
// otherwise-valid artifacts.
func artifactSummaryFrom(v artifacts.ArtifactView) ArtifactSummary {
	createdAt, err := time.Parse(artifactCreatedAtLayout, v.CreatedAt)
	if err != nil {
		createdAt = time.Time{}
	}
	return ArtifactSummary{
		ArtifactID:   v.ArtifactID,
		Name:         v.Name,
		Description:  v.Description,
		RendererKind: v.RendererKind,
		CreatedAt:    createdAt,
	}
}

// opGetArtifact implements the get_artifact tool.
//
// Metadata is resolved via ListArtifacts rather than Artifacts().GetHead:
// GetHead's memartifact.Artifact carries no CreatedAt (that field lives on
// the memory.Entry wrapper, which ListArtifacts already reads and formats),
// so going through the same list the sibling tool uses keeps one artifact id
// reporting one CreatedAt everywhere, and doubles as the existence check.
func opGetArtifact(ctx context.Context, d Deps, act acting, in GetArtifactIn) (GetArtifactOut, error) {
	if _, err := authorizeSessionOp(ctx, d, act, permView, in.Namespace, in.Name); err != nil {
		return GetArtifactOut{}, err
	}

	scope := memory.Scope{Kind: "session", ID: in.Namespace + "/" + in.Name}
	views, err := d.Artifacts().ListArtifacts(readApprovedContext(ctx, act, scope.ID), scope)
	if err != nil {
		return GetArtifactOut{}, fmt.Errorf("mcpfront: list artifacts for get_artifact: %w", err)
	}
	var summary ArtifactSummary
	found := false
	for _, v := range views {
		if v.ArtifactID == in.ArtifactID {
			summary = artifactSummaryFrom(v)
			found = true
			break
		}
	}
	if !found {
		return GetArtifactOut{}, fmt.Errorf("mcpfront: artifact %q not found in session %s/%s", in.ArtifactID, in.Namespace, in.Name)
	}

	raw, _, err := d.FetchArtifact(ctx, in.Namespace, in.Name, in.ArtifactID)
	if err != nil {
		return GetArtifactOut{}, fmt.Errorf("mcpfront: fetch artifact content: %w", err)
	}

	out := GetArtifactOut{Artifact: summary}
	if !utf8.Valid(raw) {
		// Binary content (e.g. an image render) has no text representation
		// this tool can return; Content stays empty rather than emitting
		// mangled bytes. Logged so an operator can tell "empty on purpose"
		// from "something broke".
		d.Logger().Info("mcpfront: get_artifact content is not valid UTF-8; returning no inline content",
			"ns", in.Namespace, "name", in.Name, "artifactID", in.ArtifactID)
		return out, nil
	}
	if len(raw) <= maxArtifactContentBytes {
		out.Content = string(raw)
		return out, nil
	}
	cut := maxArtifactContentBytes
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	out.Content = string(raw[:cut])
	out.ContentTruncated = true
	return out, nil
}
