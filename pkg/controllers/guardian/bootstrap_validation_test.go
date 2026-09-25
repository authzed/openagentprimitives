package guardian_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"

	// Blank-imported so their init() registers real relsource claims —
	// TestValidation_AgreesWithTheWriteTimeGuard exercises the actual claim
	// table (pt_tag, memory_entry, infoleakage_grant) rather than an invented
	// fixture source. pkg/controllers/guardian does not otherwise depend on
	// any of these.
	//
	// Each of the three DOES have exactly one init() doing exactly one
	// relsource.Register — but two of them are not otherwise side-effect-free:
	// pttagmint transitively imports pkg/memory/kinds/pttag and
	// pkg/memory/kinds/pttagcontent, and spicedbauthorizer transitively
	// imports pkg/memory/kinds/artifact (verified via `go list -deps`); each of
	// those three has its own init() calling memory.RegisterKind (kind.go:178,
	// kind.go:80, kind.go:60 respectively). approval pulls in none of the
	// three. These registrations are inert for this file's own tagged tests —
	// nothing reachable from here reads the memory.Kind registry — and
	// memory.RegisterKind panics on a duplicate Kind name rather than
	// silently overwriting, so a real collision would fail loudly rather than
	// corrupt state. Recheck this if either package's own imports change.
	//
	// pkg/channels/channelkinds/slack is deliberately NOT imported here, even
	// though it claims slack_channel/slack_workspace/slack_usergroup/slack_user
	// (DirectorySyncSource, directory_sync_source.go:35). That package has a
	// SECOND init() (kind.go:39) that registers the Slack channel kind into the
	// process-global channelkinds registry — which
	// agentsessiongrants_controller.go's Reconcile reads (SessionRelationLinks
	// and the SchemaContributor iteration around line 757) to compose the
	// agentsession schema. This test file is untagged, so it compiles into the
	// same binary as this package's integration-tagged tests
	// (sharedenv_test.go, agentsessiongrants_idempotency_test.go,
	// agentsessiongrants_controller_test.go); importing slack here would silently
	// add it to the composed schema for all of them. Three sources already give
	// eight claimed relations across three owners — do not add slack back for
	// "completeness".
	_ "github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"  // infoleakage_grant#*
	_ "github.com/authzed/openagentprimitives/pkg/memory/pttagmint"         // pt_tag#*
	_ "github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer" // memory_entry#session, #creator
)

// This file deliberately does not blank-import
// pkg/authz/spicedb/relsource/imports — see the comment on the blank
// imports above for why (it would drag in slack's DirectorySyncSource and,
// with it, the slack channel kind, into every test sharing this untagged
// file's binary). CheckWrite/CheckDeleteFilter refuse until the table is
// marked complete, so mark it directly instead: this file's own blank
// imports already registered every claim these tests assert against.
func init() {
	relsource.MarkComplete()
}

func TestValidateSpec(t *testing.T) {
	mk := func(rels ...spiceboxv1alpha1.SpiceDBBootstrapRelationship) *spiceboxv1alpha1.SpiceDBBootstrap {
		return &spiceboxv1alpha1.SpiceDBBootstrap{
			ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "default"},
			Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
				Relationships: rels,
			},
		}
	}
	relUser := func(canonical bool, id string) spiceboxv1alpha1.SpiceDBBootstrapRelationship {
		return spiceboxv1alpha1.SpiceDBBootstrapRelationship{
			Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "group", ID: "eng"},
			Relation: "member",
			Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
				Type: "user", ID: id, Canonicalize: canonical,
			},
		}
	}

	cases := []struct {
		name       string
		boot       *spiceboxv1alpha1.SpiceDBBootstrap
		wantReason string
	}{
		{
			name: "canonicalize=true with non-user type rejects",
			boot: mk(spiceboxv1alpha1.SpiceDBBootstrapRelationship{
				Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "group", ID: "eng"},
				Relation: "member",
				Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
					Type: "group", ID: "alice@example.com", Canonicalize: true,
				},
			}),
			wantReason: spiceboxv1alpha1.ReasonCanonicalizeRequiresUserEmail,
		},
		{
			name:       "canonicalize=true with non-email id rejects",
			boot:       mk(relUser(true, "not-an-email")),
			wantReason: spiceboxv1alpha1.ReasonCanonicalizeRequiresUserEmail,
		},
		{
			name:       "canonicalize=false with user type + @ in id rejects",
			boot:       mk(relUser(false, "alice@example.com")),
			wantReason: spiceboxv1alpha1.ReasonUserSubjectNotCanonical,
		},
		{
			name: "subject.relation set with canonicalize=true rejects",
			boot: mk(spiceboxv1alpha1.SpiceDBBootstrapRelationship{
				Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "agentsession", ID: "ns/foo"},
				Relation: "participant",
				Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
					Type: "user", ID: "alice@example.com", Relation: "member", Canonicalize: true,
				},
			}),
			wantReason: spiceboxv1alpha1.ReasonSubjectSetCannotCanonicalize,
		},
		{
			name:       "valid: canonicalize=true with user/email",
			boot:       mk(relUser(true, "alice@example.com")),
			wantReason: "",
		},
		{
			name: "valid: subject-set form",
			boot: mk(spiceboxv1alpha1.SpiceDBBootstrapRelationship{
				Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "agentsession", ID: "ns/foo"},
				Relation: "participant",
				Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
					Type: "group", ID: "eng", Relation: "member",
				},
			}),
			wantReason: "",
		},
		{
			name: "wildcard + canonicalize rejects",
			boot: mk(spiceboxv1alpha1.SpiceDBBootstrapRelationship{
				Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "crm_company", ID: "123"},
				Relation: "any_user",
				Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
					Type: "user", Wildcard: true, Canonicalize: true,
				},
			}),
			wantReason: spiceboxv1alpha1.ReasonSubjectWildcardConflict,
		},
		{
			name: "wildcard + relation rejects",
			boot: mk(spiceboxv1alpha1.SpiceDBBootstrapRelationship{
				Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "crm_company", ID: "123"},
				Relation: "any_user",
				Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
					Type: "user", Wildcard: true, Relation: "member",
				},
			}),
			wantReason: spiceboxv1alpha1.ReasonSubjectWildcardConflict,
		},
		{
			name: "valid: bare wildcard subject",
			boot: mk(spiceboxv1alpha1.SpiceDBBootstrapRelationship{
				Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "crm_company", ID: "123"},
				Relation: "any_user",
				Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
					Type: "user", Wildcard: true,
				},
			}),
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := guardian.ValidateSpec(tc.boot)
			if tc.wantReason == "" {
				assert.Equal(t, "", reason, "expected ok, got: %s %q", reason, msg)
			} else {
				assert.Equal(t, tc.wantReason, reason, "reason; msg=%q", msg)
			}
		})
	}
}

func TestValidateSpec_SchemaRedeclaresUser(t *testing.T) {
	boot := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{
					{Standing: spiceboxv1alpha1.StandingSessionOnly, Name: "user"},
				},
			},
		},
	}
	reason, _ := guardian.ValidateSpec(boot)
	assert.Equal(t, spiceboxv1alpha1.ReasonCannotRedeclareUser, reason)
}

// ownershipRel builds a single relationship targeting resourceType#relation,
// with a subject shape ("group" subject, no relation, no canonicalize/
// wildcard) that always passes validateRelationship on its own — every case
// below exists to exercise the ownership check, not the subject-shape
// checks above it.
func ownershipRel(resourceType, relation string) spiceboxv1alpha1.SpiceDBBootstrapRelationship {
	return spiceboxv1alpha1.SpiceDBBootstrapRelationship{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: resourceType, ID: "x1"},
		Relation: relation,
		Subject:  spiceboxv1alpha1.SpiceDBSubjectRef{Type: "group", ID: "some-group"},
	}
}

func ownershipBoot(rel spiceboxv1alpha1.SpiceDBBootstrapRelationship) *spiceboxv1alpha1.SpiceDBBootstrap {
	return &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			Relationships: []spiceboxv1alpha1.SpiceDBBootstrapRelationship{rel},
		},
	}
}

func TestValidation_RefusesARelationAnotherSourceOwns(t *testing.T) {
	// pt_tag#session is claimed by pttagmint (pkg/memory/pttagmint), not the
	// bootstrap reconciler.
	boot := ownershipBoot(ownershipRel("pt_tag", "session"))
	reason, msg := guardian.ValidateSpec(boot)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationOwnedByAnotherSource, reason)
	assert.Contains(t, msg, "pt_tag#session")
	assert.Contains(t, msg, "pttagmint")
}

func TestValidation_AllowsAnUnclaimedRelation(t *testing.T) {
	// group#member is not claimed by any registered relsource.Source.
	boot := ownershipBoot(ownershipRel("group", "member"))
	reason, msg := guardian.ValidateSpec(boot)
	assert.Equal(t, "", reason, "expected ok, got: %s %q", reason, msg)
}

// guardedFakeWriter wraps fakeWriter (spicedbbootstrap_controller_test.go)
// with the same relsource.CheckWrite / CheckDeleteFilter guard production's
// real writer applies (spiceDBClient.Writer(spicedb.BootstrapSource), wired
// in internal/cmd/operator/main.go). Plain fakeWriter bypasses that guard
// entirely — fine for every other test in this package, since none of them
// assert on an ownership refusal actually reaching the writer — but
// TestBootstrap_OwnershipRefusalIsPerRelationship needs the refusal to
// really happen at write time, the same way it does in production, to prove
// the CLEAN relationship reaches the writer while the CLAIMED one does not.
type guardedFakeWriter struct {
	*fakeWriter
	src relsource.Source
}

func (g *guardedFakeWriter) WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	if err := relsource.CheckWrite(g.src, req.GetUpdates()); err != nil {
		return nil, err
	}
	return g.fakeWriter.WriteRelationships(ctx, req)
}

func (g *guardedFakeWriter) DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	if err := relsource.CheckDeleteFilter(g.src, req.GetRelationshipFilter()); err != nil {
		return nil, err
	}
	return g.fakeWriter.DeleteRelationships(ctx, req)
}

// TestBootstrap_OwnershipRefusalIsPerRelationship is the regression pin for
// the "Important" fix: an ownership conflict on ONE relationship must not
// invalidate the whole CR the way validateShape's checks do — the write
// path's own per-tuple refusal (relsource.CheckWrite, via guardedFakeWriter
// above) is what actually stops that one relationship, exactly as in
// production, while the CR's other relationship keeps converging.
// ownershipBoot (above) builds a single-relationship CR in every OTHER test
// in this file, which cannot distinguish "the CR was refused" from "the
// relationship was refused" — they are the same thing when there is only
// one. This CR carries two: pt_tag#session (claimed by pttagmint) and
// group#member (unclaimed), each with its own distinguishable subject id.
func TestBootstrap_OwnershipRefusalIsPerRelationship(t *testing.T) {
	claimed := ownershipRel("pt_tag", "session")
	claimed.Subject.ID = "claimed-subject"
	clean := ownershipRel("group", "member")
	clean.Subject.ID = "clean-subject"
	boot := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			Relationships: []spiceboxv1alpha1.SpiceDBBootstrapRelationship{claimed, clean},
			ReclaimPolicy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete,
		},
	}

	r, c, _, w := newBootReconciler(t, boot)
	r.Writer = &guardedFakeWriter{fakeWriter: w, src: spicedb.BootstrapSource}
	reconcileBootstrapTick(t, r)

	assert.True(t, w.writesNameSubjectSince(0, "clean-subject"),
		"the clean relationship must still reach the writer")
	assert.False(t, w.writesNameSubjectSince(0, "claimed-subject"),
		"the claimed relationship must never reach the writer")

	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "b1"}, &got))
	assert.Equal(t, metav1.ConditionTrue, conditionStatus(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionValid),
		"an ownership conflict on one relationship must not invalidate the whole CR")

	appliedCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied)
	require.NotNil(t, appliedCond, "RelationshipsApplied must be stamped")
	assert.Equal(t, metav1.ConditionFalse, appliedCond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationOwnedByAnotherSource, appliedCond.Reason,
		"the specific cause (an ownership refusal), not a generic PartialApply")
	assert.Contains(t, appliedCond.Message, "pt_tag#session")
	assert.Contains(t, appliedCond.Message, "1/2 tuples applied")
}

// TestValidation_AgreesWithTheWriteTimeGuard is the reason Task 6 is safe to
// land: the per-relationship ownership check must not carry its own copy of
// the ownership rule that disagrees with the write path. It calls
// guardian.RelationshipOwnershipRefusalsForTest — a thin export_test.go
// wrapper over relationshipOwnershipRefusals, the actual function
// bootstrap_sync.go's validateBootstraps calls in production — NOT
// guardian.ValidateSpec: ValidateSpec is confirmed production-dead (its
// only callers are this package's own tests), so asserting agreement
// against it would only prove the write-time guard agrees with a function
// nothing in Reconcile's path invokes, not with what production actually
// calls.
//
// Each row builds one relationship, resolves it exactly as the write path
// does (guardian.ResolveTuple), and asserts that relationshipOwnershipRefusals'
// verdict matches relsource.CheckWrite(spicedb.BootstrapSource, ...)'s
// verdict on the same resourceType#relation — refused-when-refused and
// allowed-when-allowed, not merely "both returned something". If the two
// ever disagree, this is the test that goes red.
func TestValidation_AgreesWithTheWriteTimeGuard(t *testing.T) {
	cases := []struct {
		name         string
		resourceType string
		relation     string
	}{
		// Relations another registered source claims — CheckWrite refuses these.
		{name: "pt_tag#session claimed by pttagmint", resourceType: "pt_tag", relation: "session"},
		{name: "pt_tag#derived_from claimed by pttagmint", resourceType: "pt_tag", relation: "derived_from"},
		{name: "memory_entry#creator claimed by spicedbauthorizer", resourceType: "memory_entry", relation: "creator"},
		{name: "infoleakage_grant#audience_subject claimed by leakagegrants", resourceType: "infoleakage_grant", relation: "audience_subject"},
		{name: "agentsession#parent claimed by spicedbtypedwrites", resourceType: "agentsession", relation: "parent"},
		{name: "github_user#user claimed by spicedbtypedwrites", resourceType: "github_user", relation: "user"},

		// Relations nothing claims.
		{name: "group#member unclaimed on an unclaimed type", resourceType: "group", relation: "member"},

		// The type carries OTHER claims, but this specific relation does not —
		// only a delete filter's could-match rule treats a claimed type as
		// fully owned; a targeted write/validate does not.
		{name: "agentsession#participant unclaimed though agentsession carries other claims", resourceType: "agentsession", relation: "participant"},
		{name: "memory_entry#viewer unclaimed though memory_entry carries other claims", resourceType: "memory_entry", relation: "viewer"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := ownershipRel(tc.resourceType, tc.relation)
			boot := ownershipBoot(rel)

			refused := guardian.RelationshipOwnershipRefusalsForTest(boot)

			// Independently resolve the same relationship through the write
			// path's own resolver and ask the write-time guard directly — this
			// must NOT reuse validateRelationshipOwnership itself, or the test
			// would only prove the function agrees with a call to itself.
			tup := guardian.ResolveTuple(rel)
			update := &v1.RelationshipUpdate{
				Relationship: &v1.Relationship{
					Resource: &v1.ObjectReference{ObjectType: tup.ResourceType},
					Relation: tup.Relation,
				},
			}
			wantErr := relsource.CheckWrite(spicedb.BootstrapSource, []*v1.RelationshipUpdate{update})

			if wantErr != nil {
				require.Len(t, refused, 1,
					"CheckWrite refuses %s#%s (%v) but relationshipOwnershipRefusals allowed it", tup.ResourceType, tup.Relation, wantErr)
				assert.Contains(t, refused[0], tup.ResourceType+"#"+tup.Relation)
			} else {
				assert.Empty(t, refused,
					"CheckWrite allows %s#%s but relationshipOwnershipRefusals refused it: %v", tup.ResourceType, tup.Relation, refused)
			}
		})
	}
}
