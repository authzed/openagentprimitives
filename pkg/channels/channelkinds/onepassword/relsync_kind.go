package onepassword

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// KindName is the spec.kind value that selects this kind.
const KindName = "onepassword"

// onepasswordGroupResourceType is the only relsync.Scope.ResourceType this
// kind enumerates. Unlike the Slack kind, no cross-resource tuple rides
// along: onepassword_group.member (declared in the base scaffold,
// pkg/authz/spicedb/schema/schema.zed) relates directly to
// `user`, so FetchScope writes exactly one relation, onepassword_group#member
// — see DirectorySyncSource's claims (directory_sync_source.go) and
// TestKind_WritesOnlyWhatItClaims.
const onepasswordGroupResourceType = "onepassword_group"

// SCIM Bridge endpoints, per 1Password's own endpoint reference — NOT
// version-prefixed, despite the generic SCIM convention of "/scim/v2/...".
const (
	scimGroupsPath = "/scim/Groups"
	scimUsersPath  = "/scim/Users"
)

// scimPageSize bounds one page of a List/Query response (RFC 7644
// §3.4.2.4's "count"). Sent on /scim/Groups (ListScopes) and on the singular
// group GET, where it is also the threshold at which a member page with no
// totalResults is judged unbounded rather than complete — see
// fetchAllGroupMembers.
const scimPageSize = 100

// SyncKind is the relsync.Kind for 1Password's SCIM Bridge: it walks
// GET /scim/Groups to enumerate managed groups, GET /scim/Groups/{id} to
// read one group's membership, and GET /scim/Users/{id} to resolve a
// member to an email — turning that into the tuples DirectorySyncSource
// claims. Registered under KindName ("onepassword").
//
// Every group /scim/Groups reports is one this deployment's admin opted
// into the "Managed Groups" set (see the findings doc's "Operational
// caveat") — the same shape as the Slack kind's own limit (a bot only sees
// channels it's invited to): enumeration covers the managed/opted-in
// subset, not necessarily every group in the 1Password account.
//
// # The bridge's endpoint rides on SourceParams, not on this type
//
// The bridge is customer-hosted, so its address is per-install and cannot be
// a package constant the way Slack's fixed API host is.
// RelationshipSourceSpec.BaseURL (pkg/apis/v1alpha1/relationshipsource_types.go)
// carries the per-install value; the controller resolves it onto
// relsync.SourceParams.Endpoint (pkg/platform/relsync/kind.go) before calling
// this kind. It rides on SourceParams rather than a field on *SyncKind
// because this type is a process-wide registered singleton (init() below) —
// a per-install value stored here would be shared across every
// RelationshipSource CR naming "onepassword", which two different installs
// would fight over. ListScopes and FetchScope both fail closed with
// errEndpointUnset when creds.Endpoint is empty, rather than silently
// dereferencing an empty host or hitting the wrong install.
type SyncKind struct {
	// HTTPClient defaults to http.DefaultClient when nil.
	//
	// Deliberately NOT pkg/x/safehttp.Client(). That guarded dialer refuses
	// any IsPrivate() or loopback destination — which is exactly what an
	// in-cluster SCIM Bridge (http://scim.example.svc.cluster.local) or a
	// test/dev bridge (http://127.0.0.1:<port>) resolves to, and those are
	// the primary deployment shapes this kind exists to reach, not an edge
	// case. Wrapping this client in safehttp.Client() would block the feature
	// while looking like hardening.
	//
	// That is NOT a claim that the destination is trusted. creds.Endpoint is
	// RelationshipSourceSpec.BaseURL, which anyone who can write the CR
	// chooses, and this client sends the resolved credential to it as a bearer
	// token. What makes that safe is the check the CONTROLLER does before the
	// endpoint ever reaches this type: the credential must declare
	// allowedHosts and admit this host (see resolveCreds in
	// pkg/controllers/relationshipsource). This kind could not do it — it
	// receives relsync.SourceParams{Token, Endpoint} and never sees the
	// AgentCredential to compare against.
	HTTPClient *http.Client

	// userCache memoizes GET /scim/Users/{id} responses across FetchScope
	// calls that share the same credential — see onepasswordUserCache's own
	// doc. A member present in many groups must not be re-resolved per
	// group; the whole resumability design exists to survive rate limits.
	userCache onepasswordUserCache
}

func init() { relsync.Register(&SyncKind{}) }

// Name is the spec.kind value that selects this kind.
func (k *SyncKind) Name() string { return KindName }

// Source returns the already-registered DirectorySyncSource
// (directory_sync_source.go).
func (k *SyncKind) Source() relsource.Source { return DirectorySyncSource }

var _ relsync.ScopeLabeler = (*SyncKind)(nil)

// ScopeLabelBridges points the console at the #label tuple FetchScope writes,
// so a Directory row reads `demo-engineers` instead of a bare SCIM UUID — see
// relsync.ScopeLabeler.
//
// SCIM keys a Group by an opaque id and carries the human name in a separate
// `displayName` attribute, which this kind read on every group fetch and threw
// away. Storing it makes the name available to a reader; spicedb.ShapeNameOnSubject
// says the scope is the tuple's RESOURCE and the encoded name is its subject,
// which is the opposite arrangement from the GitHub kind's URL bridge.
//
// The decoder is DecoderB64Text, not DecoderB64URL: a displayName is free-form
// text an account admin typed, so it resolves to a title and never to an href.
func (k *SyncKind) ScopeLabelBridges() []spicedb.ScopeLabelBridge {
	return []spicedb.ScopeLabelBridge{{
		ScopeDefinition:  onepasswordGroupResourceType,
		BridgeDefinition: relsource.LabelSubjectType,
		BridgeRelation:   relsource.LabelRelation,
		Shape:            spicedb.ShapeNameOnSubject,
		Decoder:          resourcedisplay.DecoderB64Text,
	}}
}

// errEndpointUnset is returned by ListScopes/FetchScope when
// creds.Endpoint is empty — see SyncKind's own doc on why the endpoint
// rides on SourceParams rather than a field here. Failing closed with a
// named error beats a confusing "connection refused" against an empty
// host, or worse, a request to the wrong bridge.
var errEndpointUnset = errors.New("onepassword: no endpoint configured (relsync.SourceParams.Endpoint is empty) — the SCIM Bridge is customer-hosted and has no default")

// errGroupNotFound is this package's internal signal that GET
// /scim/Groups/{id} returned 404 — translated to relsync.ErrScopeGone at
// FetchScope's boundary, never returned across it directly.
var errGroupNotFound = errors.New("onepassword: group not found upstream")

// errMemberPageUnbounded is returned when GET /scim/Groups/{id} fills the
// requested member count without a totalResults that PROVES more remain,
// leaving the membership unreadable in full. Absent, zero, and a value equal to
// what is already held are the same non-answer. See fetchAllGroupMembers for
// why this is a refusal and not a best-effort read.
var errMemberPageUnbounded = errors.New("onepassword: group member page is unbounded")

// errMemberPaginationStalled is returned when a bridge keeps claiming more
// members remain while serving a page that contributes no new member ids — a
// singular GET ignoring startIndex. Refusing beats both the alternatives:
// looping forever, and reporting a subset that the per-scope prune then makes
// the whole truth.
var errMemberPaginationStalled = errors.New("onepassword: group member pagination is not advancing")

// httpClient returns k.HTTPClient, or http.DefaultClient when unset.
func (k *SyncKind) httpClient() *http.Client {
	if k.HTTPClient != nil {
		return k.HTTPClient
	}
	return http.DefaultClient
}

// doGet issues an authenticated GET against path (relative to
// creds.Endpoint), with query appended if non-empty. Auth is
// "Authorization: Bearer <token>" — the bearer token minted alongside the
// bridge's scimsession file at setup time (findings doc, "Auth header").
// The caller owns closing the response body.
func (k *SyncKind) doGet(ctx context.Context, creds relsync.SourceParams, path string, query url.Values) (*http.Response, error) {
	u := strings.TrimRight(creds.Endpoint, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("onepassword: build request for %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(creds.Token.UnderlyingValue()))
	return k.httpClient().Do(req)
}

// scimListResponse is the RFC 7644 §3.4.2 List Response envelope, as
// returned by GET /scim/Groups.
type scimListResponse struct {
	TotalResults int         `json:"totalResults"`
	ItemsPerPage int         `json:"itemsPerPage"`
	StartIndex   int         `json:"startIndex"`
	Resources    []scimGroup `json:"Resources"`
}

// scimGroup is the SCIM core Group resource (RFC 7643 §4.2) — the fields
// this kind reads out of a /scim/Groups List entry.
type scimGroup struct {
	ID          string       `json:"id"`
	DisplayName string       `json:"displayName"`
	Members     []scimMember `json:"members"`
}

// scimMember is one entry of a Group's `members` multi-valued attribute
// (RFC 7643 §4.2): "value" carries the member's User id.
type scimMember struct {
	Value string `json:"value"`
}

// scimGroupResource is the singular GET /scim/Groups/{id} response: a SCIM
// core Group resource (RFC 7643 §4.2) carrying `members`.
//
// The totalResults/itemsPerPage/startIndex triple is decoded OPPORTUNISTICALLY
// and is normally absent. Those are LIST RESPONSE fields (RFC 7644 §3.4.2), and
// RFC 7644 §3.4.1 defines no pagination for a singular resource fetch — so the
// conformant response here carries none of them, and a bridge that does carry
// them is volunteering a way to page an oversized membership.
//
// Absence does NOT degrade safely, which is the correction this doc exists to
// record. The fields decode to zero, and a loop that reads "zero" as "nothing
// more remains" returns after one page — so a group larger than one page synced
// as its first 100 members, and the per-scope prune deleted the rest.
// fetchAllGroupMembers therefore refuses a FULL page that carries no
// totalResults rather than treating it as complete; see its own doc.
type scimGroupResource struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"displayName"`
	Members      []scimMember `json:"members"`
	TotalResults int          `json:"totalResults"`
	ItemsPerPage int          `json:"itemsPerPage"`
	StartIndex   int          `json:"startIndex"`
}

// scimUserResource is the SCIM core User resource (RFC 7643 §4.1.2) — only
// the fields this kind's identity join needs.
type scimUserResource struct {
	ID     string      `json:"id"`
	Emails []scimEmail `json:"emails"`
}

// scimEmail is one entry of a User's `emails` multi-valued attribute.
type scimEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary"`
}

// ListScopes pages GET /scim/Groups, returning one relsync.Scope per group
// SCIM reports.
//
// # Cursor shape: an offset, not an opaque token
//
// relsync.Cursor.Token here is a plain decimal SCIM startIndex (RFC 7644
// §3.4.2.4), NOT an upstream-opaque handle the way the Slack kind's Token
// is (Slack's conversations.list cursor is whatever slack-go handed back,
// never parsed by that package). This kind parses its Token back to an int
// on the way in and recomputes the next one as startIndex+itemsPerPage on
// the way out. It is still tagged with Cursor.Kind == KindName and refused
// by ForKind when foreign — that half of the contract is identical to
// Slack's — but the VALUE inside it is structurally different, which is why
// this comment exists at the call site rather than assuming a reader who
// has read the Slack kind already knows how to read this one.
func (k *SyncKind) ListScopes(ctx context.Context, creds relsync.SourceParams, after relsync.Cursor) (relsync.ScopePage, error) {
	token, err := after.ForKind(k.Name())
	if err != nil {
		return relsync.ScopePage{}, err
	}
	if creds.Endpoint == "" {
		return relsync.ScopePage{}, errEndpointUnset
	}

	startIndex := 1
	if token != "" {
		n, convErr := strconv.Atoi(token)
		if convErr != nil || n < 1 {
			return relsync.ScopePage{}, fmt.Errorf("onepassword: cursor token %q is not a valid SCIM startIndex: %w", token, convErr)
		}
		startIndex = n
	}

	page, err := k.getGroupsPage(ctx, creds, startIndex, scimPageSize)
	if err != nil {
		return relsync.ScopePage{}, err
	}

	scopes := make([]relsync.Scope, 0, len(page.Resources))
	for _, g := range page.Resources {
		scopes = append(scopes, relsync.Scope{ID: relsync.ScopeID(g.ID), ResourceType: onepasswordGroupResourceType})
	}

	nextStart := startIndex + len(page.Resources)
	complete := listPageIsComplete(len(page.Resources), scimPageSize, nextStart, page.TotalResults)
	var next relsync.Cursor
	if !complete {
		next = relsync.Cursor{Kind: k.Name(), Token: strconv.Itoa(nextStart)}
	}
	return relsync.ScopePage{Scopes: scopes, Next: next, Complete: complete}, nil
}

// listPageIsComplete decides whether a /scim/Groups page is the END of the
// enumeration. It is the single most consequential boolean in this kind:
// relsync.Pass gates its whole-type reap on ScopePage.Complete, so a wrong
// `true` here does not merely stop paging — it deletes every onepassword_group
// the enumeration never reached.
//
// A FULL page is never the end, whatever totalResults says. totalResults is
// optional under RFC 7644 (§3.4.2.4 makes it a response attribute, not a
// requirement), and a bridge that omits it — or reports it per-page — is
// conformant. Absent, it decodes to 0, and any predicate comparing the next
// startIndex against it reads 101 > 0 and calls the FIRST full page the end.
// That is the reap, armed by a field the server never promised to send.
//
// The trade is deliberate and cheap: a full page that happens to exhaust the
// directory costs ONE extra request, which returns zero resources and reports
// complete on the next call. The other direction costs an operator their
// directory.
//
// Short and empty pages are what keep that extra request bounded. An empty page
// has nothing left to page toward. A short page means the server returned fewer
// than it was asked for, which is the end — unless totalResults is present AND
// says otherwise, which is the truncated case
// (TestKind_TruncatedListReportsIncomplete) that must keep reporting
// incomplete.
func listPageIsComplete(returned, requested, nextStart, totalResults int) bool {
	if returned == 0 {
		return true
	}
	if returned >= requested {
		return false
	}
	return totalResults <= 0 || nextStart > totalResults
}

// getGroupsPage issues one GET /scim/Groups?startIndex=&count= call.
func (k *SyncKind) getGroupsPage(ctx context.Context, creds relsync.SourceParams, startIndex, count int) (scimListResponse, error) {
	q := url.Values{}
	q.Set("startIndex", strconv.Itoa(startIndex))
	q.Set("count", strconv.Itoa(count))

	resp, err := k.doGet(ctx, creds, scimGroupsPath, q)
	if err != nil {
		return scimListResponse{}, fmt.Errorf("onepassword: GET %s: %w", scimGroupsPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return scimListResponse{}, fmt.Errorf("onepassword: GET %s: unexpected status %d", scimGroupsPath, resp.StatusCode)
	}
	var out scimListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return scimListResponse{}, fmt.Errorf("onepassword: decode %s response: %w", scimGroupsPath, err)
	}
	return out, nil
}

// FetchScope resolves one group's current membership: fetchAllGroupMembers
// (paged to completion, or an error — see its own doc) for the member id
// list, then one GET /scim/Users/{id} per member (memoized) to join each to
// a platform user by canonicalized email. Every step either succeeds
// outright or the whole call returns an error and an empty ScopeContent —
// see relsync.Kind.FetchScope's own atomicity doc and
// TestKind_FetchScopeIsAtomicAcrossPages.
func (k *SyncKind) FetchScope(ctx context.Context, creds relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	if creds.Endpoint == "" {
		return relsync.ScopeContent{}, errEndpointUnset
	}
	groupID := string(s.ID)

	memberIDs, displayName, err := k.fetchAllGroupMembers(ctx, creds, groupID)
	if err != nil {
		if errors.Is(err, errGroupNotFound) {
			return relsync.ScopeContent{}, relsync.ErrScopeGone
		}
		return relsync.ScopeContent{}, fmt.Errorf("onepassword: fetch group %s members: %w", groupID, err)
	}

	tokenFP := tokenFingerprint(creds.Token.UnderlyingValue())

	var tuples []spicedb.Tuple
	var joinMisses int
	for _, uid := range memberIDs {
		email, err := k.resolveMemberEmail(ctx, creds, tokenFP, uid)
		if err != nil {
			return relsync.ScopeContent{}, fmt.Errorf("onepassword: resolve member %s: %w", uid, err)
		}
		if email == "" {
			// Resolves to nobody: dropped, not written, but counted — a
			// silent partial membership is exactly what the no-silent-errors
			// rule (and ScopeContent.JoinMisses' own doc) forbids. Covers
			// both a User resource with no email attribute and a member id
			// the Users endpoint no longer recognizes at all (the group's
			// own membership list can outrun the Users directory — see
			// resolveMemberEmail).
			joinMisses++
			continue
		}
		// FromExternal, not EmailReference: this email was read from
		// 1Password's own Users endpoint inside the installed tenant — a
		// directory read behind the bridge's bearer token IS a
		// channel-verified join, the same case FromExternal's own doc
		// names, and the same reasoning the Slack kind's FetchScope
		// documents at its own identical call. TeamScope is left empty:
		// 1Password's SCIM Bridge belongs to exactly one account per
		// deployment, there is no multi-tenant scope to carry, and
		// TeamScope only affects the synthetic (email-less) encoding this
		// call never takes — a member with no email is dropped above, never
		// passed here.
		canonical, err := identity.FromExternal(identity.Kind(KindName), "", identity.RawExternalID(uid), identity.Email(email)).Canonical()
		if err != nil {
			// Unreachable today (Canonical only errors on an empty email
			// with no AllowSynthetic opt-in, and email is non-empty on this
			// branch) — fail the whole scope rather than silently drop, in
			// case that contract ever changes under us.
			return relsync.ScopeContent{}, fmt.Errorf("onepassword: canonicalize member %s email: %w", uid, err)
		}

		tuples = append(tuples, spicedb.Tuple{
			ResourceType: onepasswordGroupResourceType,
			ResourceID:   groupID,
			Relation:     "member",
			SubjectType:  "user",
			SubjectID:    canonical.String(),
		})
	}

	// The group's own SCIM displayName, for the console's Directory panel. It
	// rides on the same GET /scim/Groups/{id} response fetchAllGroupMembers
	// already reads, so naming a row costs no extra request. Ordinary scope
	// content, hashed and diffed with the memberships; see
	// relsync.ScopeLabelTuple for why that, and not the sentinel's machinery,
	// is what makes an already-synced group pick up a name.
	if labelTuple, ok := relsync.ScopeLabelTuple(ctx, s, displayName); ok {
		tuples = append(tuples, labelTuple)
	}

	return relsync.ScopeContent{Tuples: tuples, JoinMisses: joinMisses}, nil
}

// fetchAllGroupMembers reads one group's complete membership from GET
// /scim/Groups/{id}, or returns an error. An error on any page, INCLUDING the
// first, returns nothing accumulated so far and the error unwrapped
// (errGroupNotFound included): FetchScope's caller must never see a partial
// membership list, only a whole one or an error, because the per-scope prune
// deletes whatever this call does not report (kind.go's FetchScope atomicity
// doc).
//
// # Why a full page with no totalResults is refused rather than accepted
//
// totalResults is a LIST RESPONSE field (RFC 7644 §3.4.2). A singular resource
// GET returns the Group resource itself — `members` and no envelope — so it
// does not carry one, and it decodes to 0. Any predicate of the form
// `next > totalResults` therefore reads "done" after the first page, and a
// 250-member group reports 100 members. FetchScope is authoritative for the
// scope, so the per-scope prune deletes the other 150 memberships: people lose
// access, and nothing says so.
//
// A full member page is unbounded unless totalResults PROVES more remain —
// exceeds what is already held. Absent, zero, and "equal to what I just gave
// you" are all the same non-answer: a truncated list and a complete one are
// indistinguishable on the wire, so this refuses. The scope fails loudly, on
// the CR's Ready condition and in monitoring; a truncation surfaces nowhere. A
// group whose whole membership arrives in one SHORT body (the ordinary case)
// is unaffected: there is nothing left to page toward.
//
// # Why the dedupe is not an optimization
//
// A bridge that ignores startIndex on a singular GET serves page one forever
// while totalResults keeps claiming more remain. Counting ids would loop until
// the context expired; returning what accumulated would report each member
// once and look exactly like a correct short read, with the rest pruned. A
// page contributing zero NEW ids while more are claimed is neither, and is
// named as such.
// # displayName rides out with the members
//
// It is read from the FIRST page, before the empty-membership early return, and
// it is the same GET this function was already making — so the console's name
// for a group costs no extra request and an EMPTY group still gets one. Taken
// from the first page rather than the last because SCIM's own pagination is
// over `members`, not over the resource: every page repeats the group's
// attributes, and a bridge that only populates them once populates them there.
// Empty is a legitimate answer (a bridge that omits the attribute), and
// FetchScope's caller renders the raw id for it.
func (k *SyncKind) fetchAllGroupMembers(ctx context.Context, creds relsync.SourceParams, groupID string) (memberIDs []string, displayName string, err error) {
	var ids []string
	seen := make(map[string]struct{})
	// Whether totalResults has, at any point in THIS read, exceeded what was
	// held — i.e. behaved like a grand total rather than per-page bookkeeping.
	// Once it has, a later page that merely equals the accumulated count is the
	// end of a well-behaved read, not an unbounded one. See its use below.
	proven := false
	startIndex := 1
	for {
		page, err := k.getGroupPage(ctx, creds, groupID, startIndex, scimPageSize)
		if err != nil {
			return nil, "", err
		}
		if displayName == "" {
			displayName = page.DisplayName
		}
		returned := len(page.Members)
		if returned == 0 {
			return ids, displayName, nil
		}
		added := 0
		for _, m := range page.Members {
			if _, dup := seen[m.Value]; dup {
				continue
			}
			seen[m.Value] = struct{}{}
			ids = append(ids, m.Value)
			added++
		}
		// Checked BEFORE the completion arithmetic, not after: a page that
		// repeats what the last one already gave is a bridge ignoring
		// startIndex, and whether its own totalResults happens to run out on
		// this iteration decides nothing about that. Letting the arithmetic
		// win would return a subset — which the prune then makes the whole
		// truth — for exactly the bridge whose numbers cannot be trusted.
		// Unreachable on the first page, where seen is empty and a non-empty
		// page always contributes.
		if added == 0 {
			return nil, "", fmt.Errorf(
				"%w: group %s returned %d members at startIndex %d, none of them new, while claiming %d in total",
				errMemberPaginationStalled, groupID, returned, startIndex, page.TotalResults)
		}
		// A full page is unbounded unless totalResults PROVES more remain —
		// i.e. exceeds what is now held. "Absent or zero" was the weaker
		// reading and it left the truncation intact for a bridge reporting
		// per-page bookkeeping: `totalResults: 100` alongside 100 members says
		// "100 of 100", the arithmetic below then called the read finished, and
		// a 250-member group synced as 100 with the prune deleting the rest.
		// Per-page reporting is conformant (the same reason ListScopes cannot
		// trust it either), so a value that merely equals what is already held
		// carries no information about a larger group.
		//
		// A group of exactly scimPageSize with a truthful totalResults is
		// refused by this same rule. That is not collateral damage: on the wire
		// it is byte-for-byte the case above.
		//
		// `proven` is why this is decided once per READ and not once per page.
		// A group sized an exact multiple of scimPageSize ends on a full page
		// whose honest totalResults equals what is now held — arithmetic
		// indistinguishable from per-page bookkeeping if the page is judged
		// alone, but the value already proved itself earlier in this same read
		// (a 200-member group reports 200 while page one holds 100). Judging
		// each page in isolation failed those groups on every pass, so they
		// never synced at all — worse than the truncation this refuses, which
		// at least writes someone.
		if returned >= scimPageSize && !proven && page.TotalResults <= len(ids) {
			return nil, "", fmt.Errorf(
				"%w: group %s returned %d members (a full page of %d) with totalResults=%d, which does not "+
					"prove more remain, so its membership cannot be read completely; reporting a partial "+
					"list would prune the rest",
				errMemberPageUnbounded, groupID, returned, scimPageSize, page.TotalResults)
		}
		// Set AFTER the refusal, never before: a page proves the total for the
		// pages that follow it, not for itself.
		if page.TotalResults > len(ids) {
			proven = true
		}
		next := startIndex + returned
		if page.TotalResults <= 0 || next > page.TotalResults {
			return ids, displayName, nil
		}
		startIndex = next
	}
}

// getGroupPage issues one GET /scim/Groups/{id}?startIndex=&count= call.
// A 404 maps to errGroupNotFound — FetchScope's boundary, not this one,
// translates that to relsync.ErrScopeGone, keeping the ErrScopeGone
// vocabulary at the Kind interface's edge rather than scattered through
// this file's internals.
func (k *SyncKind) getGroupPage(ctx context.Context, creds relsync.SourceParams, groupID string, startIndex, count int) (scimGroupResource, error) {
	q := url.Values{}
	q.Set("startIndex", strconv.Itoa(startIndex))
	q.Set("count", strconv.Itoa(count))

	path := scimGroupsPath + "/" + url.PathEscape(groupID)
	resp, err := k.doGet(ctx, creds, path, q)
	if err != nil {
		return scimGroupResource{}, fmt.Errorf("onepassword: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return scimGroupResource{}, errGroupNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return scimGroupResource{}, fmt.Errorf("onepassword: GET %s: unexpected status %d", path, resp.StatusCode)
	}
	var out scimGroupResource
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return scimGroupResource{}, fmt.Errorf("onepassword: decode group %s response: %w", groupID, err)
	}
	return out, nil
}

// resolveMemberEmail resolves one SCIM user id to its primary email,
// memoized in k.userCache for the life of this *SyncKind. A 404 (the
// group's own membership list outrunning the Users directory — a departed
// member still listed) resolves to "", the same as a User resource with no
// email attribute: both are join misses at FetchScope's call site, never a
// hard error.
func (k *SyncKind) resolveMemberEmail(ctx context.Context, creds relsync.SourceParams, tokenFP, userID string) (string, error) {
	if email, ok := k.userCache.get(tokenFP, userID); ok {
		return email, nil
	}

	path := scimUsersPath + "/" + url.PathEscape(userID)
	resp, err := k.doGet(ctx, creds, path, nil)
	if err != nil {
		return "", fmt.Errorf("onepassword: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		k.userCache.put(tokenFP, userID, "")
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("onepassword: GET %s: unexpected status %d", path, resp.StatusCode)
	}
	var out scimUserResource
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("onepassword: decode user %s response: %w", userID, err)
	}

	email := primaryEmail(out.Emails)
	k.userCache.put(tokenFP, userID, email)
	return email, nil
}

// primaryEmail picks the entry marked primary; failing that, the first
// non-empty value; failing that, "" (no email, i.e. a join miss upstream).
func primaryEmail(emails []scimEmail) string {
	var fallback string
	for _, e := range emails {
		v := strings.TrimSpace(e.Value)
		if v == "" {
			continue
		}
		if e.Primary {
			return v
		}
		if fallback == "" {
			fallback = v
		}
	}
	return fallback
}

// onepasswordUserCacheMax bounds onepasswordUserCache's total entries — the
// same crude cap the Slack kind's own users.info cache uses (see
// slackUserInfoCacheMax's doc): this cache's job is "don't resolve the same
// member twice in one pass", not long-term identity storage.
const onepasswordUserCacheMax = 8192

// onepasswordUserCache memoizes GET /scim/Users/{id} email resolutions
// across FetchScope calls sharing the same credential — the exact case the
// resumability design exists to survive rate limits for: without it, a
// member present in dozens of groups is resolved once per group instead of
// once per pass. Keyed by a fingerprint of the resolved token (never the
// token bytes themselves) plus the SCIM user id, so two different
// installs sharing this package's one registered *SyncKind singleton (see
// relsync.Get) never see each other's cached resolution.
//
// A cached "" (no email) is a real, distinct cache entry — not "not yet
// looked up" — which is why get's second return is the presence bool
// rather than checking the string for emptiness.
type onepasswordUserCache struct {
	mu   sync.Mutex
	byID map[string]string
}

func (c *onepasswordUserCache) get(fingerprint, userID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.byID[fingerprint+":"+userID]
	return v, ok
}

func (c *onepasswordUserCache) put(fingerprint, userID, email string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string]string{}
	}
	if len(c.byID) >= onepasswordUserCacheMax {
		c.byID = map[string]string{}
	}
	c.byID[fingerprint+":"+userID] = email
}

// tokenFingerprint derives a cache-scoping key from a credential's raw
// token bytes without ever storing or comparing the token itself.
func tokenFingerprint(tok []byte) string {
	sum := sha256.Sum256(tok)
	return hex.EncodeToString(sum[:])
}
