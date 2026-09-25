package schema_test

import (
	"strings"
	"testing"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	"github.com/authzed/openagentprimitives/toolkits"
)

// subjectRef is one entry from a relation's AllowedDirectRelations: the
// object type it accepts, and — when the accepted type is itself a
// subject-SET (`foo#bar` rather than bare `foo`) — the relation on that type.
// A bare `foo` compiles to relation "..." (SpiceDB's ellipsis marker for "the
// object itself"); see ellipsisRelation in references.go.
type subjectRef struct {
	namespace string
	relation  string
}

// ellipsisSubjectRelation mirrors the unexported ellipsisRelation constant in
// references.go — duplicated here because this file lives in the external
// schema_test package and cannot reach an unexported production identifier.
// It is SpiceDB's marker for "the object itself, not a subject set", which is
// what a bare `relation member: github_user` compiles to on member's
// AllowedDirectRelations.
const ellipsisSubjectRelation = "..."

// arrowsConsuming returns every "<def>#<perm>" whose expression contains an
// arrow (rel->x / rel.any(x) / rel.all(x)) whose tupleset relation accepts ANY
// relation of objType as a subject type — i.e. a permission that TRAVERSES (or
// directly includes) objType's members, regardless of which of objType's own
// relations admitted them. It is arrowsConsumingRelation with relation "".
func arrowsConsuming(t *testing.T, src, objType string) []string {
	t.Helper()
	return arrowsConsumingRelation(t, src, objType, "")
}

// arrowsConsumingRelation is the relation-aware form: a consumer is flagged
// only when the accepted subject type is objType AND, when relation is
// non-empty, objType's relation actually reached equals it exactly. relation
// "" matches any relation of objType (arrowsConsuming's behavior).
//
// "The relation actually reached" is resolved two different ways depending on
// the shape, and conflating them is exactly the bug this comment exists to
// head off:
//
//   - A computed userset over a relation typed as a subject-SET
//     (`relation direct_member: github_user#user`) reaches exactly the
//     relation named in that type — the subject REF's own `relation` field.
//   - An arrow (`relation member: github_user` + `permission x = member->user`)
//     reaches the relation named on the ARROW's right-hand (computed) half,
//     never the subject ref's own relation field — a bare `github_user`
//     reference compiles to the ellipsis "...", which names no relation at
//     all. A filter that checked only the subject ref's relation therefore
//     silently dropped every arrow consumer once scoped to a specific
//     relation, because "..." never equals "user" or "sole_user".
//
// github_user carries two relations with different authorization postures —
// #user (agentsession membership only) and #sole_user (repo-authority
// bearing, once a directory sync wires a consumer) — and a detector that
// could not tell them apart could not keep the two separately policed. See
// pkg/controllers/useridentity/attested_edge.go.
//
// It re-implements the traversal guardian/schema's own refWalker performs,
// because that walker is unexported and this lives in the external test
// package. Public compiler + core-proto accessors are enough.
func arrowsConsumingRelation(t *testing.T, src, objType, relation string) []string {
	t.Helper()
	compiled, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("github-user-inert"),
		SchemaString: src,
	}, compiler.AllowUnprefixedObjectType())
	require.NoError(t, err, "the composed schema must compile")

	var consumers []string
	for _, def := range compiled.OrderedDefinitions {
		ns, ok := def.(*core.NamespaceDefinition)
		if !ok {
			continue
		}
		// relation name → the (namespace, relation) subject refs it accepts.
		subjectTypes := map[string][]subjectRef{}
		for _, rel := range ns.GetRelation() {
			for _, adr := range rel.GetTypeInformation().GetAllowedDirectRelations() {
				subjectTypes[rel.GetName()] = append(subjectTypes[rel.GetName()], subjectRef{
					namespace: adr.GetNamespace(),
					relation:  adr.GetRelation(),
				})
			}
		}
		for _, rel := range ns.GetRelation() {
			rw := rel.GetUsersetRewrite()
			if rw == nil {
				continue // a direct relation, not a permission
			}
			var walk func(*core.UsersetRewrite)
			// flagIfConsumes flags def#perm as a consumer when localRel's
			// declared subject types include objType at the requested relation.
			// arrowComputed is the arrow's right-hand relation name (empty for a
			// plain computed userset, which has no right-hand half) — see this
			// function's own doc comment on why it must be threaded in rather
			// than resolved solely from the subject ref's own relation field.
			flagIfConsumes := func(localRel, arrowComputed string) {
				for _, target := range subjectTypes[localRel] {
					if target.namespace != objType {
						continue
					}
					if relation != "" {
						// A subject-set-typed ref (`github_user#user`) reaches its
						// OWN relation directly. A bare ref (ellipsis "...") reaches
						// whatever the arrow's computed half names instead — the
						// bare type carries no relation of its own to compare.
						isBareRef := target.relation == "" || target.relation == ellipsisSubjectRelation
						matchesDirect := target.relation == relation
						matchesArrow := isBareRef && arrowComputed == relation
						if !matchesDirect && !matchesArrow {
							continue
						}
					}
					consumers = append(consumers, ns.GetName()+"#"+rel.GetName())
				}
			}
			walkChild := func(c *core.SetOperation_Child) {
				switch {
				case c.GetTupleToUserset() != nil:
					// An arrow rel->x: the tupleset relation is traversed, and x
					// (the computed userset) names the relation actually reached.
					ttu := c.GetTupleToUserset()
					flagIfConsumes(ttu.GetTupleset().GetRelation(), ttu.GetComputedUserset().GetRelation())
				case c.GetFunctionedTupleToUserset() != nil:
					fttu := c.GetFunctionedTupleToUserset()
					flagIfConsumes(fttu.GetTupleset().GetRelation(), fttu.GetComputedUserset().GetRelation())
				case c.GetComputedUserset() != nil:
					// A computed userset `perm = rel`: if rel is a subject
					// relation typed github_user#user, the permission resolves to
					// include those members directly — a consumer with no arrow,
					// so there is no separate computed half to pass.
					flagIfConsumes(c.GetComputedUserset().GetRelation(), "")
				}
			}
			walk = func(r *core.UsersetRewrite) {
				if r == nil {
					return
				}
				for _, op := range []*core.SetOperation{r.GetUnion(), r.GetIntersection(), r.GetExclusion()} {
					if op == nil {
						continue
					}
					for _, c := range op.GetChild() {
						walkChild(c)
						if c.GetUsersetRewrite() != nil {
							walk(c.GetUsersetRewrite())
						}
					}
				}
			}
			walk(rw)
		}
	}
	return consumers
}

// github_user#user has exactly ONE reviewed authorization consumer —
// agentsession membership — and the useridentity reconciler's collision
// handling depends on that list staying closed: it writes a SECOND binding
// (notice, not veto) when a different subject already claims a forge account
// (pkg/controllers/useridentity/attested_edge.go, SAFETY DEPENDENCY comment,
// which points HERE).
//
// This is scoped to the #user RELATION, not the github_user TYPE: #sole_user
// carries a different authorization posture on purpose (repository roles are
// meant to traverse it once a directory sync wires that consumer) and must
// stay outside this check, or the tripwire would refuse the very consumer the
// split exists to allow.
//
// The agentsession consumer is the github kind's SessionRelationLinks entry:
// the composer unions github_user#user into owner/participant/denied so a
// pull-request-triggered session can name its PR author as an owner by GitHub
// account. That consumer was reviewed against the collision policy and the
// policy KEPT, for three reasons: a second claimant of one forge account is
// normally the SAME human's second platform subject (two channels, two
// subjects), or two people who both hold the account's own verified credential
// — never a mere name-squatter, because the edge is minted only from a
// verified token; what it confers is standing on that account's OWN
// pull-request sessions, attributable and session-scoped, not repository
// access; and composer parity puts the link type in `denied` too, so what can
// be granted through it can be rescinded through it.
//
// Any OTHER consumer — above all a directory-sync join into a repo-access path
// (`permission x = team_members->user`, where team_members accepts BARE
// github_user objects and the arrow's own right half, `user`, is what ties it
// to this test's #user scope — see arrowsConsumingRelation's doc comment) —
// still breaks this test by design: a second-claimant edge would then confer
// the first claimant's github-derived REPOSITORY authority, and the collision
// handling must be revisited first (veto, or gate the consumer on
// single-binding).
//
// Composed the way production composes — base scaffold + every builtin
// TOOLKIT fragment + the real github channel-kind fragment + the kind's own
// session links — so a consumer added in any of those surfaces is caught.
//
// The toolkit fragments are the half this test was missing, and missing them
// made it inert: the repo-access join lives in toolkits/gh.yaml, and Task 2
// moved github_user to the scaffold and left the channel kind's fragment
// empty, so composing without the toolkits left no github_user consumer in
// the composed text beyond the session links the test passes itself. Doctoring
// gh.yaml's role arrows from #sole_user to #user now reports six consumers;
// before, it reported none. They go in the BASELINE (channelKindFragments)
// slot, mirroring the production composer: agentsessiongrants_controller.go's
// `schemaBaseline` is gatherChannelKindFragments() UNIONED with the built-in
// toolkit fragments, while `fragments` (the mcpFragments slot) carries only
// the accepted CR-sourced candidates — MCPServer, SidecarToolbox,
// SpiceboxToolkit, SpiceDBBootstrap. Toolkit fragments are compile-time, not
// cluster-sourced, so the baseline slot is where they actually land; passing
// them as mcpFragments here produced the same composed text today (both
// slots feed the same merge) but no longer mirrored what the controller
// constructs, which is the whole point of this fixture matching production.
func TestGithubUserBinding_OnlyAgentsessionConsumesIt(t *testing.T) {
	k := &github.Kind{}
	frag := k.SpiceDBSchemaFragment()
	require.NotNil(t, frag)

	composed, err := schema.ComposeAll(nil,
		append([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag}, schema.ToolkitFragments(toolkits.All())...),
		nil, k.SessionRelationLinks()...)
	require.NoError(t, err)

	consumers := arrowsConsumingRelation(t, composed, "github_user", "user")
	require.NotEmpty(t, consumers,
		"the composed schema must show the agentsession membership consumer; its absence means "+
			"the session link stopped composing and PR authors silently lost session ownership")
	for _, c := range consumers {
		assert.Truef(t, strings.HasPrefix(c, "agentsession#"),
			"%s consumes github_user#user, and only agentsession membership may: any other consumer "+
				"hands a second attested-identity claimant github-derived authority — revisit the "+
				"collision veto in pkg/controllers/useridentity/attested_edge.go before adding one", c)
	}
}

// Without the session links the binding itself stays inert: the fragment
// declares no permission and the base scaffold traverses nothing of
// github_user's. This is the half the useridentity reconciler's shared-bot
// refusal and the fragment-level TestKind_FragmentGrantsNoPermission rest on —
// authority enters ONLY through the composer's reviewed link union, never from
// the type itself.
func TestGithubUserBinding_InertWithoutSessionLinks(t *testing.T) {
	frag := (&github.Kind{}).SpiceDBSchemaFragment()
	require.NotNil(t, frag)

	composed, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag})
	require.NoError(t, err)

	consumers := arrowsConsuming(t, composed, "github_user")
	require.Empty(t, consumers)
}

// The detector itself must catch a consumer, or the invariant above asserts
// nothing — the same "prove the test can fail" discipline the finding relied on.
// A synthetic definition that arrows through a github_user-typed relation is
// exactly the shape the real risk takes.
func TestArrowsConsuming_DetectsAConsumer(t *testing.T) {
	frag := (&github.Kind{}).SpiceDBSchemaFragment()
	consumerFrag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition gh_team {
	relation member: github_user
	permission read = member->user
}
`,
	}
	composed, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag, consumerFrag})
	require.NoError(t, err)

	consumers := arrowsConsuming(t, composed, "github_user")
	require.Contains(t, consumers, "gh_team#read",
		"the detector must see an arrow that traverses github_user, or the inertness test is vacuous")
}

// The bug this guards against: flagIfConsumes used to compare the SUBJECT
// REF's own relation against the scope for every shape, which is correct for
// a computed userset over `relationX: github_user#user` but wrong for an
// arrow. `relation member: github_user` compiles to the ellipsis "...", and
// the relation actually consumed is the arrow's RIGHT half (`user` in
// `member->user`) — which the old code never read. Scoped to "user", every
// arrow consumer was silently filtered out, so a repo role written as
// `permission write = team_members->user` would consume github_user#user from
// OUTSIDE agentsession and TestGithubUserBinding_OnlyAgentsessionConsumesIt
// would stay green. Reuses TestArrowsConsuming_DetectsAConsumer's own schema
// so the two tests stay in lockstep.
func TestArrowsConsumingRelation_DetectsAnArrowConsumerScopedToItsComputedRelation(t *testing.T) {
	frag := (&github.Kind{}).SpiceDBSchemaFragment()
	consumerFrag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition gh_team {
	relation member: github_user
	permission read = member->user
}
`,
	}
	composed, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag, consumerFrag})
	require.NoError(t, err)

	assert.Contains(t, arrowsConsumingRelation(t, composed, "github_user", "user"), "gh_team#read",
		"an arrow's computed relation ('user' in member->user) must scope-match #user, or a real "+
			"#user consumer silently escapes TestGithubUserBinding_OnlyAgentsessionConsumesIt")
	assert.NotContains(t, arrowsConsumingRelation(t, composed, "github_user", "sole_user"), "gh_team#read",
		"the same arrow reads #user, not #sole_user, and must not be reported against that scope")
}

// The other consumer shape: a COMPUTED USERSET over a subject relation typed
// `github_user#user`. `permission members = direct_member` where
// `relation direct_member: github_user#user` resolves to include the bound
// users directly — no arrow — and confers the first claimant's authority on the
// colliding second claimant exactly as the arrow form does. A detector that
// only walks arrows would leave the invariant vacuous for this shape.
func TestArrowsConsuming_DetectsAComputedUsersetConsumer(t *testing.T) {
	frag := (&github.Kind{}).SpiceDBSchemaFragment()
	consumerFrag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition gh_group {
	relation direct_member: github_user#user
	permission members = direct_member
}
`,
	}
	composed, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag, consumerFrag})
	require.NoError(t, err)

	consumers := arrowsConsuming(t, composed, "github_user")
	require.Contains(t, consumers, "gh_group#members",
		"a computed userset over a github_user#user relation traverses it and must be detected")
}

// The point of the relation-aware narrowing: a consumer of github_user#sole_user
// (the shape repository roles will take once a directory sync wires them) must
// NOT trip TestGithubUserBinding_OnlyAgentsessionConsumesIt's #user-scoped
// check, or the split would forbid the very consumer it exists to allow. Proven
// directly here rather than left to follow from the filter's implementation.
func TestArrowsConsumingRelation_SoleUserConsumerDoesNotTripUserInvariant(t *testing.T) {
	frag := (&github.Kind{}).SpiceDBSchemaFragment()
	consumerFrag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition gh_repo_role {
	relation direct_member: github_user#sole_user
	permission members = direct_member
}
`,
	}
	composed, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag, consumerFrag})
	require.NoError(t, err)

	assert.Contains(t, arrowsConsumingRelation(t, composed, "github_user", "sole_user"), "gh_repo_role#members",
		"the detector must see a consumer of #sole_user when scoped to that relation")
	assert.NotContains(t, arrowsConsumingRelation(t, composed, "github_user", "user"), "gh_repo_role#members",
		"a #sole_user consumer must not be reported against the #user-scoped invariant")
}
