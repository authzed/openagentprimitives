package preferences

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestResolve(t *testing.T) {
	schema := []v1alpha1.UserPreferenceSchema{
		{Name: "language", Type: "enum",
			Enum:    []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}},
			Default: jsp(`"en"`)},
		{Name: "terse", Type: "bool", Default: jsp(`false`)},
		{Name: "project", Type: "string"}, // no default
	}
	cases := []struct {
		name       string
		globals    map[string]v1alpha1.PreferenceGlobal
		user       map[string]apiextv1.JSON
		key        string
		wantValue  string // raw JSON; "" = nil Value
		wantSource Source
		wantLocked bool
		wantNote   bool
	}{
		{name: "nothing set: class default wins", key: "language", wantValue: `"en"`, wantSource: SourceDefault},
		{name: "no default, nothing set: unset", key: "project", wantValue: "", wantSource: SourceUnset},
		{name: "user beats default", user: map[string]apiextv1.JSON{"language": js(`"de"`)},
			key: "language", wantValue: `"de"`, wantSource: SourceUser},
		{name: "user beats unlocked global",
			globals: map[string]v1alpha1.PreferenceGlobal{"language": {Value: js(`"en"`)}},
			user:    map[string]apiextv1.JSON{"language": js(`"de"`)},
			key:     "language", wantValue: `"de"`, wantSource: SourceUser},
		{name: "global beats default when user unset",
			globals: map[string]v1alpha1.PreferenceGlobal{"terse": {Value: js(`true`)}},
			key:     "terse", wantValue: `true`, wantSource: SourceGlobal},
		{name: "locked global beats user; Locked+Note set",
			globals: map[string]v1alpha1.PreferenceGlobal{"language": {Value: js(`"en"`), Lock: true}},
			user:    map[string]apiextv1.JSON{"language": js(`"de"`)},
			key:     "language", wantValue: `"en"`, wantSource: SourceLocked, wantLocked: true, wantNote: true},
		{name: "malformed locked global: ignored loudly, user wins",
			globals: map[string]v1alpha1.PreferenceGlobal{"language": {Value: js(`"xx"`), Lock: true}},
			user:    map[string]apiextv1.JSON{"language": js(`"de"`)},
			key:     "language", wantValue: `"de"`, wantSource: SourceUser, wantNote: true},
		{name: "schema-invalidated user value: treated absent with note",
			user: map[string]apiextv1.JSON{"language": js(`"fr"`)}, // not in enum
			key:  "language", wantValue: `"en"`, wantSource: SourceDefault, wantNote: true},
		{name: "null user tombstone: falls through to default",
			user: map[string]apiextv1.JSON{"language": js(`null`)},
			key:  "language", wantValue: `"en"`, wantSource: SourceDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := Resolve(schema, tc.globals, tc.user)
			r := findKey(t, snap, tc.key)
			if tc.wantValue == "" {
				assert.Nil(t, r.Value)
			} else {
				require.NotNil(t, r.Value)
				assert.JSONEq(t, tc.wantValue, string(r.Value.Raw))
			}
			assert.Equal(t, tc.wantSource, r.Source)
			assert.Equal(t, tc.wantLocked, r.Locked)
			if tc.wantNote {
				assert.NotEmpty(t, r.Note)
			}
		})
	}
	t.Run("stored key the schema no longer declares: ignored, not in snapshot", func(t *testing.T) {
		snap := Resolve(schema, nil, map[string]apiextv1.JSON{"gone": js(`"x"`)})
		for _, k := range snap.Keys {
			assert.NotEqual(t, "gone", k.Name)
		}
	})
	t.Run("global for an undeclared key: snapshot violation", func(t *testing.T) {
		snap := Resolve(schema, map[string]v1alpha1.PreferenceGlobal{"gone": {Value: js(`"x"`)}}, nil)
		assert.NotEmpty(t, snap.Violations)
	})
	t.Run("two undeclared global keys: violations in sorted order", func(t *testing.T) {
		snap := Resolve(schema, map[string]v1alpha1.PreferenceGlobal{
			"zebra": {Value: js(`"x"`)},
			"alpha": {Value: js(`"x"`)},
		}, nil)
		assert.Equal(t, []string{
			`classUserPreferences.alpha: key is not declared in the class's userPreferences`,
			`classUserPreferences.zebra: key is not declared in the class's userPreferences`,
		}, snap.Violations)
	})
}

// TestResolve_Visibility checks that Resolved.Visibility carries the
// schema's declared value through, normalized to "self" when the class
// author left it unset.
func TestResolve_Visibility(t *testing.T) {
	schema := []v1alpha1.UserPreferenceSchema{
		{Name: "language", Type: "string"},                          // unset
		{Name: "explicit-self", Type: "string", Visibility: "self"}, // already self
		{Name: "notify-opt-out", Type: "bool", Visibility: "class"},
		{Name: "sneaky", Type: "bool", Visibility: "everyone"}, // not a legal value
	}
	snap := Resolve(schema, nil, nil)
	assert.Equal(t, "self", findKey(t, snap, "language").Visibility, "unset visibility normalizes to self")
	assert.Equal(t, "self", findKey(t, snap, "explicit-self").Visibility)
	assert.Equal(t, "class", findKey(t, snap, "notify-opt-out").Visibility)
	assert.Equal(t, "self", findKey(t, snap, "sneaky").Visibility,
		"anything other than class fails closed to self, even a value ValidateSchema would reject")
}

func findKey(t *testing.T, s Snapshot, name string) Resolved {
	t.Helper()
	for _, k := range s.Keys {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("key %q not in snapshot", name)
	return Resolved{}
}
