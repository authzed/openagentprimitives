package trifecta_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
)

var child = authz.SessionRef{Namespace: "ns", Name: "child"}

// descriptorWith builds a surface entry at one impact tier.
func descriptorWith(impact authz.StateImpact) permsurface.Descriptor {
	return permsurface.Descriptor{StateImpact: impact}
}

// depsWith answers the three lookups from fixtures.
func depsWith(untrusted map[string]bool, readers map[string][]string, audience []string) trifecta.Deps {
	return trifecta.Deps{
		TagCarriesUntrusted: func(_ context.Context, tagID string) (bool, error) {
			return untrusted[tagID], nil
		},
		TagReaders: func(_ context.Context, tagID string) ([]string, error) {
			return readers[tagID], nil
		},
		ChildAudience: func(context.Context, authz.SessionRef) ([]string, error) {
			return audience, nil
		},
	}
}

// TestLegA_UntrustedIsAnyBoundInput: the leg is true when ANY bound input
// carries untrusted content. One hostile source is enough; the others being
// clean does not dilute it.
func TestLegA_UntrustedIsAnyBoundInput(t *testing.T) {
	d := depsWith(map[string]bool{"ptt-web": true}, nil, nil)
	h := trifecta.Handoff{Child: child, BoundInputs: []string{"ptt-db", "ptt-web"}}

	legs, err := trifecta.Derive(context.Background(), d, h)
	require.NoError(t, err)
	assert.True(t, legs.Untrusted, "one untrusted input taints the handoff; a clean sibling does not dilute it")
}

func TestLegA_AllCleanInputsIsNotUntrusted(t *testing.T) {
	d := depsWith(map[string]bool{}, nil, nil)
	h := trifecta.Handoff{Child: child, BoundInputs: []string{"ptt-db"}}

	legs, err := trifecta.Derive(context.Background(), d, h)
	require.NoError(t, err)
	assert.False(t, legs.Untrusted)
}

// TestLegB_SensitiveIsAReaderSetNarrowerThanTheChild.
//
// The question is not "is this data secret" but "is it narrower than where it
// is going". A tag everyone on the child can already read is not sensitive
// ACCESS in this sense, however confidential it feels.
func TestLegB_SensitiveIsAReaderSetNarrowerThanTheChild(t *testing.T) {
	// The child is visible to tim and sarah; the tag is readable by tim only.
	// sarah would newly see it, so the access is narrower than the child.
	d := depsWith(nil,
		map[string][]string{"ptt-secret": {"user:tim"}},
		[]string{"user:tim", "user:sarah"})
	h := trifecta.Handoff{Child: child, BoundInputs: []string{"ptt-secret"}}

	legs, err := trifecta.Derive(context.Background(), d, h)
	require.NoError(t, err)
	assert.True(t, legs.Sensitive)
}

func TestLegB_ATagTheChildsAudienceAlreadyCoversIsNotSensitive(t *testing.T) {
	d := depsWith(nil,
		map[string][]string{"ptt-open": {"user:tim", "user:sarah", "user:sam"}},
		[]string{"user:tim", "user:sarah"})
	h := trifecta.Handoff{Child: child, BoundInputs: []string{"ptt-open"}}

	legs, err := trifecta.Derive(context.Background(), d, h)
	require.NoError(t, err)
	assert.False(t, legs.Sensitive,
		"everyone who can see the child could already read it, so binding it discloses nothing")
}

// TestLegC_ConsequentialIsReadwriteOrExternalOnly.
//
// READONLY is on the surface and is NOT leg C. Being able to read is leg B's
// territory; leg C asks whether the child can ACT. Treating any surface
// membership as consequential would fire the trifecta on a child that can only
// look at things, which is the combination the design explicitly permits.
func TestLegC_ConsequentialIsReadwriteOrExternalOnly(t *testing.T) {
	cases := []struct {
		name   string
		impact authz.StateImpact
		want   bool
	}{
		{name: "readonly is not consequential", impact: authz.Readonly, want: false},
		{name: "readwrite is consequential", impact: authz.Readwrite, want: true},
		{name: "external is consequential", impact: authz.External, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := trifecta.Handoff{
				Child:        child,
				ChildSurface: []permsurface.Descriptor{descriptorWith(tc.impact)},
			}
			legs, err := trifecta.Derive(context.Background(), depsWith(nil, nil, nil), h)
			require.NoError(t, err)
			assert.Equal(t, tc.want, legs.Consequential)
		})
	}
}

// TestLegC_AnEmptySurfaceCannotAct pins what absence means, and it is only
// trustworthy because the envelope escalation shipped.
//
// permsurface.Enumerate EXCLUDES a candidate that cannot mint a handle. Before
// tool.FatalFor, such a tool was absent from the surface yet still dispatchable
// — so "empty surface" did not mean "cannot act" and this leg would have read
// clean for a child that could write. The runner now refuses to start in that
// state when the surface is enforced, which is what lets absence be read as
// inability here.
func TestLegC_AnEmptySurfaceCannotAct(t *testing.T) {
	h := trifecta.Handoff{Child: child}
	legs, err := trifecta.Derive(context.Background(), depsWith(nil, nil, nil), h)
	require.NoError(t, err)
	assert.False(t, legs.Consequential)
}

// TestAllThreeLegsTogether is the shape the judgement acts on.
func TestAllThreeLegsTogether(t *testing.T) {
	d := depsWith(
		map[string]bool{"ptt-web": true},
		map[string][]string{"ptt-web": {"user:tim"}},
		[]string{"user:tim", "user:sarah"})
	h := trifecta.Handoff{
		Child:        child,
		BoundInputs:  []string{"ptt-web"},
		ChildSurface: []permsurface.Descriptor{descriptorWith(authz.External)},
	}

	legs, err := trifecta.Derive(context.Background(), d, h)
	require.NoError(t, err)
	assert.True(t, legs.Untrusted)
	assert.True(t, legs.Sensitive)
	assert.True(t, legs.Consequential)
}

// TestAnUnresolvableLegIsAnErrorNeverFalse is the most important test here.
//
// A leg that silently reads false is the trifecta FAILING TO FIRE: no denial,
// no hold, no log, nothing anywhere to notice. Every lookup failure must
// surface, so the caller decides what an unknown means rather than inheriting
// "safe" by accident.
func TestAnUnresolvableLegIsAnErrorNeverFalse(t *testing.T) {
	boom := errors.New("spicedb unavailable")

	cases := []struct {
		name string
		deps trifecta.Deps
	}{
		{
			name: "integrity unresolvable",
			deps: trifecta.Deps{
				TagCarriesUntrusted: func(context.Context, string) (bool, error) { return false, boom },
				TagReaders:          func(context.Context, string) ([]string, error) { return nil, nil },
				ChildAudience:       func(context.Context, authz.SessionRef) ([]string, error) { return nil, nil },
			},
		},
		{
			name: "tag readers unresolvable",
			deps: trifecta.Deps{
				TagCarriesUntrusted: func(context.Context, string) (bool, error) { return false, nil },
				TagReaders:          func(context.Context, string) ([]string, error) { return nil, boom },
				ChildAudience:       func(context.Context, authz.SessionRef) ([]string, error) { return nil, nil },
			},
		},
		{
			name: "child audience unresolvable",
			deps: trifecta.Deps{
				TagCarriesUntrusted: func(context.Context, string) (bool, error) { return false, nil },
				TagReaders:          func(context.Context, string) ([]string, error) { return nil, nil },
				ChildAudience:       func(context.Context, authz.SessionRef) ([]string, error) { return nil, boom },
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := trifecta.Handoff{Child: child, BoundInputs: []string{"ptt-1"}}
			_, err := trifecta.Derive(context.Background(), tc.deps, h)
			require.Error(t, err,
				"a leg that could not be established must surface, never read as false — a false leg is the trifecta failing to fire silently")
		})
	}
}

// TestDeriveIsPolicyFree keeps this package a derivation.
//
// It returns three booleans and no verdict. The judgement lives in evaluate.go
// and the enforcement decision in the hook, so a caller can log legs without
// being handed a refusal, and the containment tripper can reach a different
// conclusion from the same facts.
func TestDeriveIsPolicyFree(t *testing.T) {
	d := depsWith(
		map[string]bool{"ptt-web": true},
		map[string][]string{"ptt-web": {"user:tim"}},
		[]string{"user:tim", "user:sarah"})
	h := trifecta.Handoff{
		Child:        child,
		BoundInputs:  []string{"ptt-web"},
		ChildSurface: []permsurface.Descriptor{descriptorWith(authz.External)},
	}

	legs, err := trifecta.Derive(context.Background(), d, h)
	require.NoError(t, err)
	assert.True(t, legs.All(), "All is a reading of the legs, not a decision about them")
}
