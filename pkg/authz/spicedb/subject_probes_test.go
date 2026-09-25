//go:build !integration && !e2e

// pkg/authz/spicedb/subject_probes_test.go
//
// UNIT-ONLY, and the build constraint above is load-bearing, not tidiness.
//
// relsource's registry is process-global and never reset, and these fixtures
// register from init(). Without the constraint this file's init() links into
// the -tags=integration binary too, where ListSubjectIdentities runs against a
// REAL SpiceDB holding only the bare scaffold (pkg/authz/spicedb/schema/
// schema.zed, the whole of what test/testspicedb writes). Every fixture
// definition that is not in that scaffold — probe_accepted, probe_entry — then
// fails its read on EVERY integration call and lands in Unavailable, breaking
// tests in a file that never mentions this one.
//
// That is not hypothetical: it happened. cluster#debugger and
// onepassword_group#member leaked here for as long as this file has existed and
// were harmless only by accident, because both definitions happen to be in the
// scaffold. The moment a fixture named one that was not — and the moment
// per-probe failures stopped being swallowed by first-error-wins — the leak
// became a deterministic red in the integration suite. The fix is the
// constraint, not choosing fixture names that happen to exist: a future fixture
// must be free to name anything.
package spicedb

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// probeFixtureSource is registered once, from init(), rather than inside the
// test body: kindregistry panics on a duplicate Name, so a Register call
// inside TestSubjectProbes_DerivesFromClaimsAndSkipsTheSentinel would panic
// the whole package binary (not just fail one test) the moment the test
// runs twice in one process, e.g. `go test -count=2`.
//
// Claims here deliberately avoid "github_user#user" (claimed for real by
// TypedWritesSource, registered from this same package's own
// typed_writes_source.go init()) and "memory_entry#creator" /
// "artifact#creator" (claimed by subject_identities_integration_test.go's
// fixture-directory source, which registers into this same never-reset
// registry whenever this file's tests run alongside it under
// -tags=integration). relsource.buildIndex panics on two DIFFERENT sources
// claiming the same relation, and that check fires the first time anything
// in this test binary calls CheckWrite/CheckDeleteFilter (writer_test.go
// does) — so a colliding claim here would crash unrelated tests, not just
// this one.
// probeFixtureDisplay is the fixture's DisplayName, and the name the probes it
// contributes are attributed to — Display(), not Name.
const probeFixtureDisplay = "Probe Fixture"

func init() {
	// The identity-shaped source: it OWNS three relations, but only one of them
	// is a bare user-subject link. The other two are the two shapes that must
	// never become a probe — a userset-subject membership, and the hash
	// sentinel.
	relsource.Register(relsource.Source{
		Name:        "probe-fixture",
		DisplayName: probeFixtureDisplay,
		Claims: []string{
			"cluster#debugger",
			"onepassword_group#member",
			"onepassword_group#" + relsource.SentinelRelation,
		},
		SubjectIdentityClaims: []string{"cluster#debugger", "onepassword_group#member"},
	})

	// A non-directory writer: it claims relations (so it is in the table and
	// would have been walked by the old Claims-based derivation) and declares
	// NO identity links. memory_entry#creator is the real shape this stands in
	// for — TOUCHed on every memory Put by every user.
	relsource.Register(relsource.Source{
		Name:   "probe-fixture-nondirectory",
		Claims: []string{"probe_entry#creator", "probe_entry#session"},
	})

	// The accepted half of TestRegister_RefusesAnIdentityClaimThatCouldNever
	// Match. Registered from init() for the reason at the top of this file:
	// kindregistry panics on a duplicate Name, so a Register call in a test
	// body panics the whole package binary under `go test -count=2`. The
	// REFUSED cases are safe in a test body only because Register validates
	// before it registers, so a refused source never takes its name.
	relsource.Register(relsource.Source{
		Name:                  "probe-fixture-accepted",
		Claims:                []string{"probe_accepted#user", "probe_accepted#other"},
		SubjectIdentityClaims: []string{"probe_accepted#user"},
	})
}

// The probe set is derived from each source's SubjectIdentityClaims, not from
// its Claims. Asserted on the probe SET rather than on query results, because
// both defects it guards are invisible downstream: a userset-subject probe
// returns nothing (indistinguishable from a person with no links), and a
// non-directory probe returns rows that look plausible.
func TestSubjectProbes_DeriveFromDeclaredIdentityClaimsOnly(t *testing.T) {
	probes := subjectProbes()

	var fixture []string
	for _, p := range probes {
		if p.source == probeFixtureDisplay {
			fixture = append(fixture, p.definition+"#"+p.relation)
		}
	}

	assert.ElementsMatch(t, []string{"cluster#debugger", "onepassword_group#member"}, fixture,
		"only the declared identity claims are probed — the sentinel is not, and the probe is attributed to Display()")

	// C2: a writer that claims relations but declares no identity link must
	// contribute NOTHING. Walking Claims put memory_entry#creator in the probe
	// set, so the console rendered one row per memory entry a person had ever
	// created, attributed to the memory authorizer.
	for _, p := range probes {
		assert.NotEqual(t, "probe_entry", p.definition,
			"a source with no SubjectIdentityClaims must contribute no probe (got %s#%s from %q)",
			p.definition, p.relation, p.source)
	}
}

// C3: the shapes that CANNOT match are refused where they are written, not
// silently dropped where they are read. A probe that returns nothing is
// indistinguishable from a person with no links, so a declaration that can only
// ever produce one has to fail loudly at registration instead.
func TestRegister_RefusesAnIdentityClaimThatCouldNeverMatch(t *testing.T) {
	cases := []struct {
		name   string
		src    relsource.Source
		expect string
	}{
		{
			name: "userset-subject membership not in Claims: refused as not owned",
			src: relsource.Source{
				Name:                  "probe-fixture-refused-unowned",
				Claims:                []string{"probe_unowned#identity"},
				SubjectIdentityClaims: []string{"probe_org#member"},
			},
			expect: "not one of its Claims",
		},
		{
			name: "hash sentinel: refused, its subject is a string digest",
			src: relsource.Source{
				Name:                  "probe-fixture-refused-sentinel",
				Claims:                []string{"probe_sentinel#" + relsource.SentinelRelation},
				SubjectIdentityClaims: []string{"probe_sentinel#" + relsource.SentinelRelation},
			},
			expect: "string digest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requirePanicContaining(t, tc.expect, func() { relsource.Register(tc.src) })
		})
	}

	// The mirror, registered from this file's init(): a declaration that IS a
	// subset of Claims takes effect and is probed.
	var accepted []string
	for _, p := range subjectProbes() {
		if p.definition == "probe_accepted" {
			accepted = append(accepted, p.definition+"#"+p.relation)
		}
	}
	assert.Equal(t, []string{"probe_accepted#user"}, accepted,
		"a well-formed declaration is probed, and only the declared relation is")
}

// requirePanicContaining runs fn and requires it to panic with a message
// containing want. Pinning the message (not merely "it panicked") is what
// distinguishes the intended refusal from an unrelated crash in the same call.
func requirePanicContaining(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.NotNil(t, r, "must panic")
		assert.Contains(t, fmt.Sprint(r), want)
	}()
	fn()
}

// An unmarked claim table must refuse rather than return an empty answer:
// relsource.All() coming back empty because this binary never linked
// relsource/imports is indistinguishable, from the caller's side, from a
// person who genuinely has no directory links. subjectIdentitiesGuard takes
// the completeness value as a parameter rather than calling
// relsource.IsComplete() itself for exactly this reason — the process-global
// latch is already true in this binary (writer_test.go's init() marks it,
// and MarkComplete never un-marks by design), so the only way to exercise
// the false branch is to pass false directly.
func TestSubjectIdentitiesGuard_RefusesWhenClaimTableIncomplete(t *testing.T) {
	err := subjectIdentitiesGuard(false)
	require.Error(t, err)
	assert.ErrorIs(t, err, relsource.ErrClaimTableIncomplete)
}

// The mirror case: a complete table is not refused.
func TestSubjectIdentitiesGuard_AllowsWhenClaimTableComplete(t *testing.T) {
	assert.NoError(t, subjectIdentitiesGuard(true))
}
