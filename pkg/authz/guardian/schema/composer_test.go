package schema_test

import (
	"context"
	"sync"
	"testing"

	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/generator"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// baseSchema is the "pre-slice-2" schema text — what we expect to find
// in SpiceDB before composition runs. It defines github_repo (with
// read/admin) and agentsession (with started_by/participant/interact)
// but no grant_* relations.
//
// The `use expiration` directive at the top enables the
// `... with caveat and expiration` syntax that composed grant_* relations
// emit; without it SpiceDB v1.49 rejects the produced schema text.
const baseSchema = `
use expiration

caveat check_hash(arguments_hash string, allowed_arguments_hash string) {
    arguments_hash == allowed_arguments_hash
}

definition user {}

definition github_repo {
    relation reader: user
    relation admin: user
    permission read = reader + admin
    permission admin = admin
}

definition agentsession {
    relation started_by: user
    relation participant: user
    permission interact = started_by + participant
}
`

// assertContainsAll fails the test if `got` is missing any of the
// expected substrings. Each missing string produces its own assertion
// failure so the test prints all gaps at once.
func assertContainsAll(t *testing.T, got string, wants []string, msg string) {
	t.Helper()
	for _, want := range wants {
		assert.Containsf(t, got, want, "%s: missing %q", msg, want)
	}
}

// identify wraps bare fragments as the CR-sourced ComposeAll/RunAll param now
// takes. These tests don't exercise fragment identity, so Key is left zero —
// an empty Key only changes the internal synthetic-filesystem name
// composeFragmentSet assigns each fragment (it falls into the "baseline"
// naming scheme). That CAN change the composed TEXT once two or more RawZed
// fragments are in play — Key decides definition ORDER (see
// composeFragmentSet's own doc) — but every fixture in this file passes at
// most one fragment, so there is nothing to reorder against here.
// TestComposeAll_FragmentKeyAndTierAreIdentityMetadataOnly in
// fragmentcompose_equivalence_test.go is where the ordering effect and its
// canonical-equivalence limit are actually pinned.
func identify(frags ...*spiceboxv1alpha1.SpiceDBSchemaFragment) []schema.IdentifiedFragment {
	out := make([]schema.IdentifiedFragment, 0, len(frags))
	for _, f := range frags {
		out = append(out, schema.IdentifiedFragment{Fragment: f})
	}
	return out
}

func TestComposeAddsGrantPairs(t *testing.T) {
	pairs := []schema.GrantPair{
		{ResourceType: "github_repo", Permission: "admin"},
		{ResourceType: "github_repo", Permission: "read"},
	}
	got, changed, err := schema.Compose(baseSchema, pairs)
	require.NoError(t, err, "Compose")
	assert.True(t, changed, "changed=true when pairs are new")
	// Spot-check the agentsession block has the two relations + permissions.
	assertContainsAll(t, got, []string{
		"relation grant_admin_github_repo: github_repo with check_hash and expiration",
		"permission check_admin_github_repo = grant_admin_github_repo->admin",
		"relation grant_read_github_repo: github_repo with check_hash and expiration",
		"permission check_read_github_repo = grant_read_github_repo->read",
	}, "added grant pairs")
	// Existing relations/permissions on agentsession preserved.
	assertContainsAll(t, got, []string{
		"relation started_by: user",
		"permission interact = started_by + participant",
	}, "preserved existing")
}

func TestComposeNoOpWhenPairsAlreadyPresent(t *testing.T) {
	pairs := []schema.GrantPair{
		{ResourceType: "github_repo", Permission: "admin"},
	}
	pass1, _, err := schema.Compose(baseSchema, pairs)
	require.NoError(t, err, "Compose pass1")
	pass2, changed, err := schema.Compose(pass1, pairs)
	require.NoError(t, err, "Compose pass2")
	assert.False(t, changed, "second pass with same pairs: changed=false")
	assert.Equal(t, pass1, pass2, "second pass must not change schema text")
}

func TestComposeSkipsPairsWithMissingDefinition(t *testing.T) {
	pairs := []schema.GrantPair{
		{ResourceType: "github_repo", Permission: "admin"}, // OK
		{ResourceType: "nonexistent", Permission: "read"},  // missing def
	}
	got, _, skipped, err := schema.ComposeWithSkipped(baseSchema, pairs)
	require.NoError(t, err, "ComposeWithSkipped")
	assert.Contains(t, got, "grant_admin_github_repo", "admin/github_repo pair added")
	assert.NotContains(t, got, "grant_read_nonexistent", "nonexistent pair must NOT be added")
	require.Len(t, skipped, 1, "skipped count")
	assert.Equal(t, "nonexistent", skipped[0].ResourceType, "skipped pair is nonexistent")
}

func TestComposeSkipsPairsWithMissingPermissionOnDefinition(t *testing.T) {
	pairs := []schema.GrantPair{
		{ResourceType: "github_repo", Permission: "write"}, // github_repo has no `write` permission
	}
	_, _, skipped, err := schema.ComposeWithSkipped(baseSchema, pairs)
	require.NoError(t, err, "ComposeWithSkipped")
	require.Len(t, skipped, 1, "skipped count")
}

// TestComposeAll_IncludesBothFragmentsAndGrants exercises the slice-4
// happy path: an MCPServer-contributed resource definition flows
// through EmitSpicedbSchema into the scaffold, and Compose injects
// grant relations for pairs targeting that resource. The output must
// contain both the fragment's `definition <name>` block and the
// composed `grant_<perm>_<rt>` relation.
func TestComposeAll_IncludesBothFragmentsAndGrants(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{
				Name: "crm_company",
				Relations: []spiceboxv1alpha1.SpiceDBRelation{
					{Name: "owner", SubjectType: "user"},
				},
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{
					{Name: "contact_access", Expr: "owner"},
				},
			},
		},
	}
	pairs := []schema.GrantPair{
		{ResourceType: "crm_company", Permission: "contact_access"},
	}
	got, err := schema.ComposeAll(identify(fragment), nil, pairs)
	require.NoError(t, err, "ComposeAll")
	assertContainsAll(t, got, []string{
		"use expiration",
		"caveat check_hash",
		"definition user {}",
		"definition crm_company {",
		"relation owner: user",
		"permission contact_access = owner",
		"definition agentsession {",
		"relation grant_contact_access_crm_company: crm_company with check_hash and expiration",
		"permission check_contact_access_crm_company = grant_contact_access_crm_company->contact_access",
	}, "ComposeAll output")
}

// TestComposeAll_NoFragmentsNoPairs verifies the scaffold-only path:
// even with zero MCPServers contributing fragments and zero
// AgentSessionGrants declaring pairs, ComposeAll must emit a valid
// SpiceDB schema containing the code-owned base (use expiration,
// check_hash caveat, agentsession definition). This is the cold-start
// state of a freshly installed cluster.
func TestComposeAll_NoFragmentsNoPairs(t *testing.T) {
	got, err := schema.ComposeAll(nil, nil, nil)
	require.NoError(t, err, "ComposeAll")
	assertContainsAll(t, got, []string{
		"use expiration",
		"caveat check_hash",
		"definition user {}",
		"definition agentsession {",
		"relation started_by: user",
		// `owner` is the session authz subject (user | group#member);
		// interact derives from owner + participant.
		"relation owner: user | group#member",
		"relation participant: user | group#member",
		"permission interact = owner + started_by + participant - denied",
		// The metaagent control-plane gate: only the session owner may change scope.
		// Checked fully-consistent at MetaagentReceived.
		"permission manage_scope = owner",
	}, "ComposeAll(nil,nil) scaffold output")
}

// TestComposer_AgentSession_HasForkPermission verifies the SessionFork
// control-plane gate's runtime permission: only the session owner may
// fork (restart-from-here). Rendered the same way the manage_scope assertion
// is (the scaffold-only ComposeAll path).
func TestComposer_AgentSession_HasForkPermission(t *testing.T) {
	out, err := schema.ComposeAll(nil, nil, nil)
	require.NoError(t, err, "ComposeAll")
	assert.Contains(t, out, "permission fork = owner",
		"agentsession must expose fork = owner (owner-only) for the SessionFork gate")
}

// TestComposeAll_IncludesArtifactDefinition is the regression guard for the
// live-view drift bug: the `artifact` definition (webd's CheckArtifactView
// gate; the spicedbauthorizer's TouchArtifactParent target) was missing from
// the operator's composed schema, so the live schema had no `artifact` type and
// every CheckArtifactView errored ("object definition `artifact` not found") →
// webd 500 on the live preview. The base scaffold (now the single canonical
// schema in pkg/authz/spicedb/schema) MUST define artifact with view deriving from the
// parent session. This test composes from that canonical, so it also guards
// against the canonical losing artifact.
func TestComposeAll_IncludesArtifactDefinition(t *testing.T) {
	out, err := schema.ComposeAll(nil, nil, nil)
	require.NoError(t, err, "ComposeAll")
	assertContainsAll(t, out, []string{
		"definition artifact {",
		"parent->interact", // permission view = parent->interact (only artifact uses this)
	}, "scaffold must define the artifact live-view resource")
}

// TestComposeAll_ManageScopeSurvivesRecompose verifies the manage_scope
// permission is preserved through a recompose that injects grant pairs over
// the scaffold (Compose's "keep existing non-grant lines" loop must not strip
// it — it starts with neither "relation grant_", "permission check_", nor a
// brace, so it qualifies as a preserved line).
func TestComposeAll_ManageScopeSurvivesRecompose(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{
				Name:        "crm_company",
				Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: "owner", SubjectType: "user"}},
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "contact_access", Expr: "owner"}},
			},
		},
	}
	pairs := []schema.GrantPair{{ResourceType: "crm_company", Permission: "contact_access"}}
	got, err := schema.ComposeAll(identify(fragment), nil, pairs)
	require.NoError(t, err, "ComposeAll with grant pairs")
	assert.Contains(t, got, "permission manage_scope = owner",
		"manage_scope must survive a recompose that injects grant_* relations")
	assert.Contains(t, got, "grant_contact_access_crm_company",
		"the grant pair must still be composed in alongside manage_scope")
}

// TestComposeAll_UseTokenSurvivesRecompose verifies the use_token permission
// and authorized_token relation are preserved through a recompose that injects
// grant pairs over the scaffold (Compose's "keep existing non-grant lines" loop
// must not strip them — they don't start with "relation grant_", "permission
// check_", nor a brace, so they qualify as preserved lines).
func TestComposeAll_UseTokenSurvivesRecompose(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{
				Name:        "mcpserver_demo",
				Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: "owner", SubjectType: "user"}},
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "call", Expr: "owner"}},
			},
		},
	}
	pairs := []schema.GrantPair{{ResourceType: "mcpserver_demo", Permission: "call"}}
	got, err := schema.ComposeAll(identify(fragment), nil, pairs)
	require.NoError(t, err, "ComposeAll with grant pairs")
	assert.Contains(t, got, "relation authorized_token: externaltoken | externaltoken with token_value_matches",
		"authorized_token relation must survive recompose")
	assert.Contains(t, got, "permission use_token = authorized_token",
		"use_token permission must survive recompose")
	assert.Contains(t, got, "definition externaltoken {}", "externaltoken definition must be present")
}

// TestRunAll_OneBadFragmentFreezesWholeCompose is a RED-confirm for the
// cross-tenant authz DoS this package's ValidateFragment (called by
// pkg/controllers/guardian BEFORE any fragment reaches RunAll) exists to
// prevent: RunAll's compose→WriteSchema is all-or-nothing over its
// WHOLE mcpFragments slice, so a single bad fragment (here, one
// redeclaring the reserved `agentsession` definition — exactly what
// ValidateFragment's reserved-name check rejects) makes RunAll error
// out even though a perfectly good fragment (`github_repo`) was in the
// same slice. Fed straight into RunAll (the pre-fix controller
// behavior — flatten every MCPServer's fragment into one slice with no
// per-fragment validation), this is a global freeze: every
// AgentSessionGrants in the cluster would see the resulting runErr and
// flip SchemaIncluded=False, not just the tenant that owns the bad
// MCPServer. The controller-level fix isolates the bad fragment BEFORE
// this call so RunAll only ever sees the good ones — see
// pkg/controllers/guardian/agentsessiongrants_controller_test.go's
// TestReconciler_IsolatesInvalidMCPServerFragment for the GREEN
// (post-fix, partitioned) behavior.
func TestRunAll_OneBadFragmentFreezesWholeCompose(t *testing.T) {
	goodFrag := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{{
		Name: "github_repo",
		Relations: []spiceboxv1alpha1.SpiceDBRelation{
			{Name: "reader", SubjectType: "user"},
		},
		Permissions: []spiceboxv1alpha1.SpiceDBPermission{
			{Name: "read", Expr: "reader"},
		},
	}}}
	badFrag := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{{
		Name:        "agentsession", // reserved: collides with the base scaffold's definition
		Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "evil", Expr: "nil"}},
	}}}
	pairs := []schema.GrantPair{{ResourceType: "github_repo", Permission: "read"}}

	io := &fakeSchemaIO{}
	_, err := schema.RunAll(context.Background(), io,
		identify(goodFrag, badFrag), nil, pairs, nil)
	require.Error(t, err, "one bad fragment in the batch must fail RunAll for the WHOLE batch — this is the bug the controller-level isolation fixes")
	assert.Equal(t, 0, io.writes, "no write should have happened; the freeze is total, not partial")
}

// TestRunAll_DoesNotWriteWhenValidationFails proves the PLUMBING: a
// composeAllWithSkipped failure — whatever produces it — must reach RunAll as
// an error and must NEVER be followed by a write. A composer that errored
// while RunAll wrote anyway would be worse than no check at all: the error
// would look like it protected the cluster while the bad schema landed
// regardless.
//
// The fixture is
// schema.SlotCollidesWithRelationFragment()/schema.GadgetViewerSlot() —
// exported by postsurgery_validate_internal_test.go specifically so this
// test does not duplicate it inline and the two cannot drift out of sync. It
// used to be schema.SlotWithoutOwnerFragment()/schema.WidgetReadSlot(), the
// shape ComposeSlots' once-unconditional `->interact + owner` rewrite made
// invalid — composeOneSlot now checks whether the target declares `owner`
// before writing that leg (see slots.go), so that fixture composes cleanly
// and no longer exercises this gate. SlotCollidesWithRelationFragment is a
// DIFFERENT genuinely-invalid-after-surgery shape (a slotted permission name
// that collides with an existing relation name) that the owner-leg fix does
// not touch — see postsurgery_validate_internal_test.go for why. This test
// exercises the SAME end-to-end path
// TestComposeAllWithSkipped_RefusesAnInvalidPostSurgeryResult constructs and
// explains there (through RunAll rather than composeAllWithSkipped directly);
// it isn't a weaker, bypass-only proof.
//
// The assertion is on the distinguishing wrapper and the specific failure,
// not merely "validate composed schema" — validateCompiled emits that phrase
// at BOTH the assembly stage (inside composeFragmentSet) and this
// post-surgery stage, so a fixture broken so it fails at assembly instead —
// never reaching the slot stage this test exists to exercise — would still
// satisfy a bare substring match on it and stay green for the wrong reason.
func TestRunAll_DoesNotWriteWhenValidationFails(t *testing.T) {
	frags := []schema.IdentifiedFragment{schema.SlotCollidesWithRelationFragment()}
	slots := []schema.SlotPair{schema.GadgetViewerSlot()}

	io := &fakeSchemaIO{}
	_, err := schema.RunAll(context.Background(), io, frags, nil, nil, slots)
	require.Error(t, err, "RunAll must surface the validation failure")
	assert.Contains(t, err.Error(), "grant/slot composition produced invalid schema",
		"must be the post-surgery gate, not merely any validateCompiled failure")
	assert.Contains(t, err.Error(), "duplicate relation/permission name `viewer` under definition `gadget`",
		"must name the specific defect the slot's insert path introduced")
	assert.Zero(t, io.writes,
		"a schema that fails validation must never be written: the previous "+
			"schema keeps serving, whereas a bad write takes authorization down "+
			"cluster-wide")
}

// A prior version of this test (TestComposeAll_FragmentKeyAndTierDoNotAffect
// ComposedOutput) asserted a stronger claim than is actually true: that Key
// and Tier never change the composed schema TEXT at all. That only held
// because its fixture passed exactly one fragment, carrying only structured
// Resources — composeFragmentSet reads Key solely to name a RawZed
// fragment's synthetic file (see that function's doc), so with one
// structured-only fragment there was nothing for Key to reorder, and the
// byte-identical assertion passed for reasons that had nothing to do with
// Key being neutral. TestComposeAll_FragmentKeyAndTierAreIdentityMetadataOnly
// in fragmentcompose_equivalence_test.go replaces it with the honest,
// two-RawZed-fragment version of the claim: Key/Tier may reorder the raw
// composed TEXT, but never change the composed SCHEMA's canonical meaning.

// TestComposeAll_FragmentConflictPropagates verifies that conflicting
// resource declarations across MCPServer fragments surface as an
// error from ComposeAll (delegated through EmitSpicedbSchema).
func TestComposeAll_FragmentConflictPropagates(t *testing.T) {
	f1 := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{
		{Name: "thing", Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "p", Expr: "a"}}},
	}}
	f2 := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{
		{Name: "thing", Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "p", Expr: "b"}}},
	}}
	_, err := schema.ComposeAll(identify(f1, f2), nil, nil)
	require.Error(t, err, "conflict must error")
	assert.Contains(t, err.Error(), "conflicting definitions", "error describes conflict")
}

// fakeSchemaIO is a minimal in-memory SchemaIO for RunAll tests.
type fakeSchemaIO struct {
	mu      sync.Mutex
	current string
	writes  int
}

// ReadSchema models real SpiceDB, which does NOT echo back the bytes it was
// given: it stores the compiled schema and re-renders it on read via
// generator.GenerateSchema, which drops comments and normalizes formatting.
// A fake that returned `current` verbatim would make every "no second write"
// assertion below vacuous — RunAll's read-back comparison would be diffing a
// string against itself, so it could never catch a comparison that only holds
// for byte-identical text. That is not hypothetical: the composed schema
// carries a large comment block, so live text is ~1.2KB shorter than what was
// written.
func (f *fakeSchemaIO) ReadSchema(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == "" {
		return "", nil
	}
	return renderLikeSpiceDB(f.current)
}

// renderLikeSpiceDB compiles src and re-renders it the way SpiceDB's
// ReadSchema does, so tests observe the same canonicalization production does.
func renderLikeSpiceDB(src string) (string, error) {
	compiled, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("fake-spicedb"),
		SchemaString: src,
	}, compiler.AllowUnprefixedObjectType())
	if err != nil {
		return "", err
	}
	out, _, err := generator.GenerateSchema(context.Background(), compiled.OrderedDefinitions)
	if err != nil {
		return "", err
	}
	return out, nil
}

func (f *fakeSchemaIO) WriteSchema(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = text
	f.writes++
	return nil
}

// TestRunAll_WritesAndIdempotent verifies RunAll writes the composed
// schema on the first call and is a no-op (zero additional writes)
// when called again with identical inputs.
func TestRunAll_WritesAndIdempotent(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{
				Name: "doc",
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{
					{Name: "read", Expr: "self"},
				},
				Relations: []spiceboxv1alpha1.SpiceDBRelation{
					{Name: "self", SubjectType: "user"},
				},
			},
		},
	}
	pairs := []schema.GrantPair{{ResourceType: "doc", Permission: "read"}}
	io := &fakeSchemaIO{}

	res, err := schema.RunAll(context.Background(), io, identify(fragment), nil, pairs, nil)
	require.NoError(t, err, "RunAll pass1")
	assert.True(t, res.Changed, "pass1: Changed=true")
	assert.Equal(t, 1, io.writes, "pass1: writes=1")
	assert.Contains(t, io.current, "grant_read_doc", "pass1: schema contains grant_read_doc")

	res, err = schema.RunAll(context.Background(), io, identify(fragment), nil, pairs, nil)
	require.NoError(t, err, "RunAll pass2")
	assert.False(t, res.Changed, "pass2: Changed=false on identical inputs")
	assert.Equal(t, 1, io.writes, "pass2: no additional write")
}

// TestRunAll_ConvergesAgainstCanonicalizedLiveSchema pins the property the
// guardian's whole reconcile loop rests on: with unchanging inputs, the
// cluster reaches a fixed point and STOPS writing.
//
// RunAll decides whether to write by comparing the live schema to the composed
// one. The composed text carries the scaffold's comment blocks; SpiceDB stores
// the compiled schema and re-renders on read, dropping them. Comparing the two
// as raw bytes therefore never reports "equal", and the controller rewrites the
// entire schema on every single reconcile — forever, at the debounce interval —
// while reporting Changed=true as though it were doing real work.
//
// Ten passes rather than two: one extra pass cannot tell a loop that converges
// late from one that never converges at all.
func TestRunAll_ConvergesAgainstCanonicalizedLiveSchema(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{
				Name:        "doc",
				Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "read", Expr: "self"}},
				Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: "self", SubjectType: "user"}},
			},
		},
	}
	pairs := []schema.GrantPair{{ResourceType: "doc", Permission: "read"}}
	io := &fakeSchemaIO{}

	for pass := 1; pass <= 10; pass++ {
		res, err := schema.RunAll(context.Background(), io,
			identify(fragment), nil, pairs, nil)
		require.NoErrorf(t, err, "RunAll pass %d", pass)
		if pass == 1 {
			assert.True(t, res.Changed, "pass 1 must write: the live schema starts empty")
			continue
		}
		assert.Falsef(t, res.Changed, "pass %d: inputs unchanged, so nothing to write", pass)
	}
	assert.Equal(t, 1, io.writes,
		"exactly one write across 10 identical passes; more means the loop never converges and rewrites the schema every reconcile in production")
	live, err := io.ReadSchema(context.Background())
	require.NoError(t, err, "read back live schema")
	assert.Contains(t, live, "grant_read_doc", "the one write must still carry the grant relation")
}

// TestRunAll_SurfaceSkippedPairs verifies pairs whose resourceType or
// permission isn't declared by any MCPServer fragment land in
// Result.SkippedPairs rather than the schema text.
func TestRunAll_SurfaceSkippedPairs(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{Name: "doc", Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "read", Expr: "self"}}, Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "self", SubjectType: "user"}}},
		},
	}
	pairs := []schema.GrantPair{
		{ResourceType: "doc", Permission: "read"},      // OK
		{ResourceType: "missing", Permission: "read"},  // skipped: no definition
		{ResourceType: "doc", Permission: "writelock"}, // skipped: no such permission
	}
	io := &fakeSchemaIO{}
	res, err := schema.RunAll(context.Background(), io, identify(fragment), nil, pairs, nil)
	require.NoError(t, err, "RunAll")
	require.Len(t, res.SkippedPairs, 2, "SkippedPairs count")
	assert.Contains(t, io.current, "grant_read_doc", "schema contains valid grant")
	// Assert on the identifiers the composer would actually emit for a skipped
	// pair (`grant_<perm>_<resType>` / `check_<perm>_<resType>`), not on the
	// bare resourceType/permission words. A bare-substring assertion also
	// matches ordinary English in the base scaffold's prose comments, so any
	// future comment containing the word "missing" would fail this test for a
	// reason that has nothing to do with pair skipping.
	assert.NotContains(t, io.current, "grant_read_missing", "schema must not include the skipped-resourceType grant")
	assert.NotContains(t, io.current, "check_read_missing", "schema must not include the skipped-resourceType check permission")
	assert.NotContains(t, io.current, "grant_writelock_doc", "schema must not include the skipped-permission grant")
	assert.NotContains(t, io.current, "check_writelock_doc", "schema must not include the skipped-permission check permission")
}

// TestComposeAll_IncludesChannelKindFragments verifies that channel-kind
// fragments passed as the second arg are included in the composed schema
// alongside the base scaffold.
func TestComposeAll_IncludesChannelKindFragments(t *testing.T) {
	var mcpFragments []schema.IdentifiedFragment
	channelKindFragments := []*spiceboxv1alpha1.SpiceDBSchemaFragment{
		{RawZed: "definition fake_channel_def {}"},
	}
	got, err := schema.ComposeAll(mcpFragments, channelKindFragments, nil)
	require.NoError(t, err)
	assert.Contains(t, got, "definition fake_channel_def {}")
}

// TestComposeAll_MixedMCPAndChannelKindFragments verifies that both MCP
// and channel-kind fragments appear together in the composed output.
func TestComposeAll_MixedMCPAndChannelKindFragments(t *testing.T) {
	mcpFragments := identify(&spiceboxv1alpha1.SpiceDBSchemaFragment{RawZed: "definition mcp_only {}"})
	channelKindFragments := []*spiceboxv1alpha1.SpiceDBSchemaFragment{
		{RawZed: "definition channel_only {}"},
	}
	got, err := schema.ComposeAll(mcpFragments, channelKindFragments, nil)
	require.NoError(t, err)
	assert.Contains(t, got, "definition mcp_only {}")
	assert.Contains(t, got, "definition channel_only {}")
}

// baseSchemaWithOwner is a variant of baseSchema carrying both the owner and
// participant relations, as the production schema does, so sessionLinks
// appending is exercised on both lines.
const baseSchemaWithOwner = `
use expiration

caveat check_hash(arguments_hash string, allowed_arguments_hash string) {
    arguments_hash == allowed_arguments_hash
}

definition user {}

definition group {
    relation member: user
    permission membership = member
}

definition github_repo {
    relation reader: user
    relation admin: user
    permission read = reader + admin
    permission admin = admin
}

definition slack_channel {
    relation member: user
}

definition slack_usergroup {
    relation member: user
}

definition agentsession {
    relation started_by: user
    relation owner: user | group#member
    relation participant: user | group#member
    permission interact = owner + participant
}
`

// TestCompose_AppendsSessionRelationLinks verifies that sessionLinks are
// unioned into BOTH the owner and participant relation lines of the
// agentsession block. The schema used here does NOT include
// slack_channel/slack_usergroup definitions, so we assert the text union
// only — ComposeWithSkipped does not compile its output (compilation of the
// final schema is SpiceDB's responsibility at write time; channel-kind
// fragments supply those definitions in production via composeAllWithSkipped /
// RunAll).
func TestCompose_AppendsSessionRelationLinks(t *testing.T) {
	links := []string{"slack_channel#member", "slack_usergroup#member"}
	out, _, _, err := schema.ComposeWithSkipped(baseSchemaWithOwner, nil, links...)
	require.NoError(t, err)
	// Both owner and participant must have the links appended (sorted).
	assert.Contains(t, out, "relation owner: user | group#member | slack_channel#member | slack_usergroup#member")
	assert.Contains(t, out, "relation participant: user | group#member | slack_channel#member | slack_usergroup#member")
}

// TestCompose_AppendsSessionRelationLinks_Idempotent verifies that running
// ComposeWithSkipped twice with the same links produces identical output
// (no duplicate segments).
func TestCompose_AppendsSessionRelationLinks_Idempotent(t *testing.T) {
	links := []string{"slack_channel#member", "slack_usergroup#member"}
	pass1, _, _, err := schema.ComposeWithSkipped(baseSchemaWithOwner, nil, links...)
	require.NoError(t, err, "pass1")
	pass2, _, _, err := schema.ComposeWithSkipped(pass1, nil, links...)
	require.NoError(t, err, "pass2")
	assert.Equal(t, pass1, pass2, "second compose with same links must not duplicate them")
}

// TestCompose_AppendsSessionRelationLinks_Sorted verifies the deterministic
// ordering guarantee: links are appended in sorted order regardless of the
// input order so the assertion is stable.
func TestCompose_AppendsSessionRelationLinks_Sorted(t *testing.T) {
	// Reversed order vs. the canonical alphabetical order.
	links := []string{"slack_usergroup#member", "slack_channel#member"}
	out, _, _, err := schema.ComposeWithSkipped(baseSchemaWithOwner, nil, links...)
	require.NoError(t, err)
	// slack_channel sorts before slack_usergroup alphabetically.
	assert.Contains(t, out, "relation owner: user | group#member | slack_channel#member | slack_usergroup#member")
}

// TestComposeAll_BadSessionLinkFailsLoudly verifies the defense-in-depth
// compile check: a SessionRelationLink referencing a type no fragment defines
// must fail compose, not silently produce invalid schema that only errors later
// at WriteSchema against live SpiceDB.
func TestComposeAll_BadSessionLinkFailsLoudly(t *testing.T) {
	_, err := schema.ComposeAll(nil, nil, nil, "undefined_type#member")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined object type")
}

// TestCompose_NoLinksIsNoOp verifies that passing no sessionLinks leaves
// the relation lines exactly as they appear in the source.
func TestCompose_NoLinksIsNoOp(t *testing.T) {
	out, _, _, err := schema.ComposeWithSkipped(baseSchemaWithOwner, nil)
	require.NoError(t, err)
	// The relation lines are left exactly as in the source — no link appended.
	assert.Contains(t, out, "relation participant: user | group#member")
	assert.NotContains(t, out, "group#member | slack_channel#member")
}

// The composer scaffold injects grant pairs ONLY — it emits no disallow
// surface (no tool_action, disallowed_tool or is_disallowed_<rt>). Hard-deny is
// enforced at Layer 2 by the runner's Scope hook over the session_scope doc.
