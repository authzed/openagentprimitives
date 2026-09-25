package spicedb

import (
	"context"
	"io"
	"sort"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
)

// ScopeLabelShape names which HALF of a declared tuple carries the name, out
// of a closed set of two. A named type rather than a bool for the same reason
// resourcedisplay.ObjectIDDecoder is one: a declaration elsewhere SELECTS a
// read this package authored, and an unrecognized value resolves nothing
// rather than guessing.
type ScopeLabelShape string

const (
	// ShapeNameOnResource is the bridged shape, and the zero value so that a
	// bridge predating this field keeps reading exactly as it did:
	//
	//	<BridgeDefinition>:<name-bearing id> #<BridgeRelation> @ <ScopeDefinition>:<scope id>
	//
	// The scope is the SUBJECT; the name rides on the resource. GitHub's
	// github_repo_url:<b64 URL>#repo@github_repo:<forge id> is this.
	ShapeNameOnResource ScopeLabelShape = ""
	// ShapeNameOnSubject is the direct shape:
	//
	//	<ScopeDefinition>:<scope id> #<BridgeRelation> @ <BridgeDefinition>:<name-bearing id>
	//
	// The scope is the RESOURCE; the name rides on the subject. A sync that
	// stores a name it fetched — rather than pointing at a URL-keyed object it
	// already had a reason to write — produces this:
	// slack_channel:C3#label@string:<base64url of the channel name>.
	ShapeNameOnSubject ScopeLabelShape = "name-on-subject"
)

// ScopeLabelBridge declares ONE relation, already written by a directory sync,
// one side of which carries the human name for a scope definition's ids.
//
// It exists because a forge's own key is rarely a name. The GitHub sync keys a
// repository scope by its numeric forge id (github_repo:1005857813), which is
// stable and correct and means nothing to a person reading a console. The sync
// ALSO writes github_repo_url:<base64url of the repo's https URL>#repo@
// github_repo:<id> — a bridge tying the URL an agent would type to the
// repository the forge enumerated — so the name was already in SpiceDB and
// this type is how a kind points at it.
//
// # Two shapes, because two syncs arrived at a name differently
//
// GitHub had a name in SpiceDB for a reason unrelated to naming rows, so
// pointing at it cost no new relation. Slack and 1Password did not: both
// FETCHED a name during a sync and discarded it, so there was nothing to point
// at, and the console rendered raw ids. Those kinds now store what they fetch
// as <scope>#label@string:<encoded name>, which inverts which half of the
// tuple is the scope — hence Shape. Everything else about this type, and
// everything downstream of it (SourceScope.Labels, LabelsUnavailable, the
// projector, the frontend), is identical for both.
//
// What a kind declares here is still only where to READ. Nothing in this
// package writes a label; the tuple is ordinary scope content the sync engine
// diffs and prunes like any other (pkg/platform/relsync).
//
// A scope definition with no entry at all is not a defect: its ids render raw,
// which is the honest answer when nothing stored anywhere holds their names.
type ScopeLabelBridge struct {
	// ScopeDefinition is the scope-bearing definition whose ids this bridge
	// names ("github_repo", "slack_channel") — the same string that appears on
	// SourceScope.Definition, which is how a resolved label finds its rows.
	ScopeDefinition string
	// BridgeDefinition is the definition whose OBJECT IDS carry the name:
	// "github_repo_url" under ShapeNameOnResource, and the permission-less
	// "string" type under ShapeNameOnSubject.
	BridgeDefinition string
	// BridgeRelation is the relation joining the two ("repo", "label"). Which
	// definition it is DECLARED on depends on Shape — see the constants.
	BridgeRelation string
	// Shape says which half of the tuple is the scope and which carries the
	// name.
	//
	// The ZERO VALUE is ShapeNameOnResource, the bridged shape, so a
	// declaration written before this field existed reads unchanged. That makes
	// it the one field here a stored-name bridge must set EXPLICITLY: omitting
	// it is not a missing value the reader can detect, it is a different valid
	// shape.
	//
	// A stored-name bridge that forgets it reads BridgeDefinition as the
	// resource type — "string", which declares no relations — so the read
	// errors and the page raises a LabelsUnavailable notice rather than
	// silently showing raw ids. That is loud, but it is loud because of what
	// `string` happens to be, not by design: a stored-name bridge whose
	// BridgeDefinition were some other real definition would resolve nothing
	// and say nothing. Set it.
	Shape ScopeLabelShape
	// Decoder turns one name-bearing object id into a title and a link href. A
	// member of resourcedisplay's closed set: a kind SELECTS a presentation, it
	// never authors one, so an id can never render as anything this repo did
	// not write the code for. An unrecognized decoder resolves nothing, which
	// degrades to raw ids rather than to garbage.
	//
	// The decoder expresses a payload's presentation, but NOT its trust: a
	// directory-authored display name is decoded by
	// resourcedisplay.DecoderB64Text, which derives a title and never an href,
	// and a declaration that named DecoderB64URL here instead would not
	// reintroduce a link — resolved() drops the href for every
	// ShapeNameOnSubject bridge regardless of what this field says. See its own
	// doc for why that is decided from Shape rather than left to this field.
	Decoder resourcedisplay.ObjectIDDecoder
}

// readDefinition is the resource type readScopeLabels actually streams for
// this bridge, which is not the same field under both shapes.
//
// Used for the failure report as well as the read itself: an UnavailableProbe
// naming "string#label" would send an operator looking at the wrong definition
// entirely — the read that failed was over slack_channel.
func (b ScopeLabelBridge) readDefinition() string {
	if b.Shape == ShapeNameOnSubject {
		return b.ScopeDefinition
	}
	return b.BridgeDefinition
}

// subjectDefinition is the subject type readScopeLabels filters on — the
// mirror of readDefinition, and the other half of the same tuple.
func (b ScopeLabelBridge) subjectDefinition() string {
	if b.Shape == ShapeNameOnSubject {
		return b.BridgeDefinition
	}
	return b.ScopeDefinition
}

// scopeAndNameIDs picks, from one streamed row's two object ids, which is the
// scope being named and which carries the name.
//
// Separated from the stream loop for the reason the joiner below already is:
// getting it backwards is SILENT. The read would run, return rows, join none
// of them (a name-bearing id never matches a scope id), and produce a page of
// raw ids — which is indistinguishable from a directory that stores no names,
// i.e. from the exact state this whole mechanism exists to leave behind. A
// live SpiceDB is not needed to catch that, and should not be required to.
func (b ScopeLabelBridge) scopeAndNameIDs(resourceID, subjectID string) (scopeID, nameID string) {
	if b.Shape == ShapeNameOnSubject {
		return resourceID, subjectID
	}
	return subjectID, resourceID
}

// ScopeLabel is one scope id's resolved presentation.
//
// Title is a name; Href is where that name points, or empty. They are one
// value rather than two parallel maps because a row that has a link but no
// name, or a name from one bridge and a link from another, is not a state
// this read can produce and should not be a state a caller has to handle.
type ScopeLabel struct {
	// Title is the human name — "demo-org/widgets" for a GitHub repository.
	// Never empty in a map this package returns: an id that resolved to no
	// title is ABSENT, so a caller's lookup miss is the single "render the
	// raw id" signal.
	Title string
	// Href is an https link target for this scope, or empty when the decoded
	// value is not linkable. Scheme-guarded by resourcedisplay.EligibleHref —
	// never http, never javascript:, never data:.
	Href string
}

// readScopeLabels resolves the human names for the scope ids in want, using
// ONE ReadRelationships over the whole bridge definition.
//
// One read, not one per scope, and that is the entire point of joining in
// memory: a bridge lookup per scope id would be 150+ round trips for a single
// console page load of an org of ordinary size. The bridge is keyed by URL
// and its SUBJECT is the scope, so there is no id filter that could narrow
// the read to the scopes we want anyway — the narrowing has to happen on this
// side.
//
// Memory stays bounded by len(want), not by the size of the bridge
// definition: a row whose subject is not a scope we are rendering is dropped
// as it streams past rather than collected. An org with 50,000 repositories
// and a cap of 100 rendered rows holds 100 entries here.
//
// An id that does not decode is simply ABSENT from the result — the caller
// renders the raw id. That is a fallback, not a failure: a malformed bridge
// id is a row we cannot name, not a read we could not do, and reporting it as
// a failed probe would put a scary notice on a page that is otherwise
// completely correct.
// Under ShapeNameOnSubject the read is over the SCOPE definition itself and
// one row is one scope, so it is bounded by scope count exactly as
// readScopeSentinels is — never by membership count, which is the number that
// actually gets large.
func (c *Client) readScopeLabels(ctx context.Context, b ScopeLabelBridge, want map[string]bool) (map[string]ScopeLabel, error) {
	join := newScopeLabelJoiner(b, want)
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		// Fully consistent for the same reason the sentinel read is: an admin
		// opening this page right after a sync must see that pass's own writes,
		// and a stale bridge would name a repository by a URL it no longer has.
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:     b.readDefinition(),
			OptionalRelation: b.BridgeRelation,
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType: b.subjectDefinition(),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		rel := resp.GetRelationship()
		subj := rel.GetSubject()
		// Which half is the scope and which carries the name is the ONLY thing
		// Shape changes. consider's own rules — the userset guard, the
		// want-filter, the tie-break — are written against these three values
		// and are identical either way, which is what keeps one joiner (and one
		// set of tests for it) covering both shapes.
		scopeID, nameID := b.scopeAndNameIDs(rel.GetResource().GetObjectId(), subj.GetObject().GetObjectId())
		join.consider(scopeID, subj.GetOptionalRelation(), nameID)
	}
	return join.resolved(), nil
}

// scopeLabelJoiner is the in-memory half of readScopeLabels: it decides which
// streamed bridge rows matter and what each surviving scope id is called.
//
// Split from the stream loop so the join, the tie-break and the
// undecodable-id fallback are reachable without a live SpiceDB — those three
// are where a wrong answer is SILENT (a row named after the wrong repository,
// a name that flickers between refreshes, a decoded blob rendered as a name),
// and none of them should be testable only through the integration harness.
type scopeLabelJoiner struct {
	bridge ScopeLabelBridge
	want   map[string]bool
	// raw holds the winning bridge object id per scope id. Decoding is
	// deferred to resolved(): decoding on arrival would decode ids that a
	// later, lower-sorting row displaces.
	raw map[string]string
}

func newScopeLabelJoiner(b ScopeLabelBridge, want map[string]bool) *scopeLabelJoiner {
	return &scopeLabelJoiner{bridge: b, want: want, raw: make(map[string]string, len(want))}
}

// consider offers one streamed bridge row to the join. Everything it drops,
// it drops as the row goes past rather than by collecting and filtering
// afterward — that is what keeps memory bounded by the number of rows being
// rendered rather than by the size of the bridge definition.
func (j *scopeLabelJoiner) consider(scopeID, subjectRelation, bridgeID string) {
	if subjectRelation != "" {
		// A USERSET subject, not the scope object itself. The bridge names a
		// bare scope object; anything else on this relation was written by
		// something other than the sync and must not name a row.
		return
	}
	if !j.want[scopeID] {
		return
	}
	// Deterministic tie-break rather than last-write-wins. The sync keeps one
	// bridge tuple per repo scope, but a stale edge can outlive a rename until
	// the next full-coverage pass reaps it, and SpiceDB's stream order is not
	// stable — without this, two refreshes of the same page could show two
	// different names for one row.
	if prev, ok := j.raw[scopeID]; ok && prev <= bridgeID {
		return
	}
	j.raw[scopeID] = bridgeID
}

// resolved decodes the winning bridge ids into names.
//
// A scope whose id does not decode is ABSENT from the result, never present
// with an empty or partial title: absence is the single signal a caller reads
// as "render the raw id", and a half-decoded blob rendered as a name would be
// worse than the forge id it replaced.
//
// # A stored name is never a link, whatever decoder was declared
//
// Under ShapeNameOnSubject the payload is text somebody typed into a directory
// — a Slack channel name, a SCIM displayName — and this drops the href
// unconditionally.
//
// It is belt-and-braces over resourcedisplay.DecoderB64Text, which already has
// no path that returns one, and it is here because Shape and Decoder are
// INDEPENDENT fields. A future kind pairing ShapeNameOnSubject with
// DecoderB64URL gets precisely the attack that decoder split exists to
// prevent: a channel named `https://evil.example/a/b` renders as anchor text
// `a/b` pointing at evil.example — an attacker-chosen label on an
// attacker-chosen target, which is the whole shape of a spoofed link. Pinning
// the pairing in each kind's own tests makes the guarantee hold by everyone
// remembering; deciding it from the SHAPE makes it hold because a
// name-on-subject payload is display text by construction and there is no
// declaration that can opt out.
//
// The bridged shape keeps its href: those ids are URLs this repo itself minted
// from a forge's canonical fields, which is exactly the case a link is for.
func (j *scopeLabelJoiner) resolved() map[string]ScopeLabel {
	out := make(map[string]ScopeLabel, len(j.raw))
	for scopeID, raw := range j.raw {
		title, href := j.bridge.Decoder.Decode(raw)
		if title == "" {
			continue
		}
		if j.bridge.Shape == ShapeNameOnSubject {
			href = ""
		}
		out[scopeID] = ScopeLabel{Title: title, Href: href}
	}
	return out
}

// resolveScopeLabels attaches a resolved name to every scope id it can, over
// every bridge, and reports the bridge reads that failed.
//
// A bridge whose ScopeDefinition synced nothing is skipped entirely: there is
// no row to name, so a read would cost a round trip to answer a question
// nobody asked — and, on a cluster where the bridge definition is not in the
// live schema yet, would raise an "unavailable" notice on a page with nothing
// missing from it.
//
// Failures are COLLECTED, never returned: the rows are already read and
// correct, and blanking them over an unresolvable name would be the worse
// answer. The caller renders the rows with raw ids AND says the names could
// not be resolved — see SourceScopes.LabelsUnavailable.
func (c *Client) resolveScopeLabels(ctx context.Context, scopes []SourceScope, bridges []ScopeLabelBridge, sourceDisplay string) (labels map[string]map[string]ScopeLabel, unavailable []UnavailableProbe) {
	wanted := make(map[string]map[string]bool, len(scopes))
	for _, s := range scopes {
		ids := make(map[string]bool, len(s.ScopeIDs))
		for _, id := range s.ScopeIDs {
			ids[id] = true
		}
		wanted[s.Definition] = ids
	}

	labels = make(map[string]map[string]ScopeLabel)
	for _, b := range bridges {
		want := wanted[b.ScopeDefinition]
		if len(want) == 0 {
			continue
		}
		resolved, err := c.readScopeLabels(ctx, b, want)
		if err != nil {
			unavailable = append(unavailable, UnavailableProbe{
				Source: sourceDisplay,
				// The definition actually READ, not BridgeDefinition — under
				// ShapeNameOnSubject those differ, and reporting "string#label"
				// would send an operator to a definition that has no relations
				// at all instead of to the one whose read fell over.
				Definition: b.readDefinition(),
				Relation:   b.BridgeRelation,
				Err:        err.Error(),
			})
			continue
		}
		merged := labels[b.ScopeDefinition]
		if merged == nil {
			merged = make(map[string]ScopeLabel, len(resolved))
			labels[b.ScopeDefinition] = merged
		}
		for id, lbl := range resolved {
			// First bridge to name a scope wins. Two bridges over one
			// definition is not a shape any kind declares today; fixing an
			// order here rather than leaving it to map iteration keeps it from
			// becoming a flicker if one ever does.
			if _, ok := merged[id]; !ok {
				merged[id] = lbl
			}
		}
	}
	sort.Slice(unavailable, func(i, j int) bool {
		if unavailable[i].Definition != unavailable[j].Definition {
			return unavailable[i].Definition < unavailable[j].Definition
		}
		return unavailable[i].Relation < unavailable[j].Relation
	})
	return labels, unavailable
}
