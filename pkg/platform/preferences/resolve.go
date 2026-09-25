package preferences

import (
	"fmt"
	"sort"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

type Source string

const (
	SourceUnset   Source = "unset"
	SourceDefault Source = "default"
	SourceGlobal  Source = "global"
	SourceUser    Source = "user"
	SourceLocked  Source = "locked"
)

type Resolved struct {
	Name        string                         `json:"name"`
	Type        string                         `json:"type"`
	Description string                         `json:"description,omitempty"`
	Enum        []v1alpha1.PreferenceEnumValue `json:"enum,omitempty"`
	Pattern     string                         `json:"pattern,omitempty"`
	Value       *apiextv1.JSON                 `json:"value,omitempty"` // nil when unset
	Source      Source                         `json:"source"`
	Locked      bool                           `json:"locked"`
	Note        string                         `json:"note,omitempty"` // violation affecting this key
	// Visibility is always "self" or "class" — never empty. Any schema value
	// other than an explicit "class" (unset included) normalizes to "self"
	// here, so every consumer compares against a concrete value and an
	// invalid schema value can never widen who reads a preference.
	Visibility string `json:"visibility"`
}

type Snapshot struct {
	Keys       []Resolved `json:"keys"`
	Violations []string   `json:"violations,omitempty"`
}

// Resolve computes the precedence-ordered effective value for each key in the schema.
// Precedence per key: locked global → user value → global → class default → unset.
// Every candidate is re-validated against the current schema.
// A null user value is the cleared tombstone and is skipped as if absent.
// Violations are loud: malformed globals are ignored with a Note,
// schema-invalidated user values are treated absent with a Note.
func Resolve(schema []v1alpha1.UserPreferenceSchema,
	globals map[string]v1alpha1.PreferenceGlobal,
	user map[string]apiextv1.JSON) Snapshot {

	snap := Snapshot{}
	declared := map[string]bool{}
	for i := range schema {
		s := schema[i]
		declared[s.Name] = true
		// Fail-closed normalization: ONLY an explicit "class" widens
		// visibility. Anything else — unset, "self", or a value the CEL enum
		// and ValidateSchema should have rejected — resolves to "self", so a
		// schema that dodged validation cannot widen who reads a value.
		visibility := "self"
		if s.Visibility == "class" {
			visibility = "class"
		}
		r := Resolved{Name: s.Name, Type: s.Type, Description: s.Description,
			Enum: s.Enum, Pattern: s.Pattern, Source: SourceUnset, Visibility: visibility}

		g, hasGlobal := globals[s.Name]
		globalValid := false
		if hasGlobal {
			if err := ValidateValue(s, g.Value); err != nil {
				// Never silently applied, never silently blocking: the bad
				// global is ignored and the violation rides the snapshot.
				r.Note = fmt.Sprintf("admin global for %q ignored: %v", s.Name, err)
				snap.Violations = append(snap.Violations,
					fmt.Sprintf("classUserPreferences.%s: %v", s.Name, err))
			} else {
				globalValid = true
			}
		}
		uv, hasUser := user[s.Name]
		if hasUser && isNull(uv) {
			hasUser = false // cleared tombstone
		}
		if hasUser {
			if err := ValidateValue(s, uv); err != nil {
				r.Note = joinNote(r.Note, fmt.Sprintf("your saved value is no longer valid and was ignored: %v", err))
				hasUser = false
			}
		}

		switch {
		case globalValid && g.Lock:
			v := g.Value
			r.Value, r.Source, r.Locked = &v, SourceLocked, true
			if hasUser {
				r.Note = joinNote(r.Note, "your saved value is overridden by admin policy")
			}
		case hasUser:
			v := uv
			r.Value, r.Source = &v, SourceUser
		case globalValid:
			v := g.Value
			r.Value, r.Source = &v, SourceGlobal
		case s.Default != nil:
			// Copy: aliasing the schema's own pointer would let a consumer
			// mutate an informer-cached object through the snapshot.
			v := *s.Default
			r.Value, r.Source = &v, SourceDefault
		}
		snap.Keys = append(snap.Keys, r)
	}
	for name := range globals {
		if !declared[name] {
			snap.Violations = append(snap.Violations,
				fmt.Sprintf("classUserPreferences.%s: key is not declared in the class's userPreferences", name))
		}
	}
	// Map iteration order is random; sort so the snapshot is deterministic.
	sort.Strings(snap.Violations)
	return snap
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
