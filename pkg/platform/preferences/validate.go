// Package preferences validates and resolves per-user agent preferences: the
// class-authored schema (AgentClass.spec.userPreferences), the platform
// admin's globals (AgentSettings.spec.classUserPreferences), and the user's
// saved values (the user_preference memory kind). Pure — no I/O.
package preferences

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ValidateSchema checks a class's userPreferences list for self-consistency.
func ValidateSchema(schema []v1alpha1.UserPreferenceSchema) error {
	seen := map[string]bool{}
	for i := range schema {
		s := schema[i]
		if seen[s.Name] {
			return fmt.Errorf("duplicate preference %q", s.Name)
		}
		seen[s.Name] = true
		if len(s.Enum) > 0 && s.Type != "enum" {
			return fmt.Errorf("preference %q: enum is only valid for type \"enum\"", s.Name)
		}
		if s.Type == "enum" && len(s.Enum) == 0 {
			return fmt.Errorf("preference %q: type \"enum\" requires enum values", s.Name)
		}
		ev := map[string]bool{}
		for _, e := range s.Enum {
			if ev[e.Value] {
				return fmt.Errorf("preference %q: duplicate enum value %q", s.Name, e.Value)
			}
			ev[e.Value] = true
		}
		if s.Pattern != "" {
			if s.Type != "string" && s.Type != "stringList" {
				return fmt.Errorf("preference %q: pattern is only valid for string and stringList", s.Name)
			}
			if _, err := regexp.Compile(s.Pattern); err != nil {
				return fmt.Errorf("preference %q: pattern: %w", s.Name, err)
			}
		}
		if s.Default != nil {
			if err := ValidateValue(s, *s.Default); err != nil {
				return fmt.Errorf("default for %q: %w", s.Name, err)
			}
		}
		if s.Visibility != "" && s.Visibility != "self" && s.Visibility != "class" {
			return fmt.Errorf("preference %q: visibility must be \"self\" or \"class\", got %q", s.Name, s.Visibility)
		}
	}
	return nil
}

// ValidateValue checks one value against one preference's declared shape.
// A JSON null is valid for every type: it is the "cleared" tombstone.
func ValidateValue(sch v1alpha1.UserPreferenceSchema, v apiextv1.JSON) error {
	if isNull(v) {
		return nil
	}
	switch sch.Type {
	case "string":
		var s string
		if err := json.Unmarshal(v.Raw, &s); err != nil {
			return fmt.Errorf("expected a string: %w", err)
		}
		return matchPattern(sch.Pattern, s)
	case "stringList":
		var items []string
		if err := json.Unmarshal(v.Raw, &items); err != nil {
			return fmt.Errorf("expected a string list: %w", err)
		}
		for _, s := range items {
			if err := matchPattern(sch.Pattern, s); err != nil {
				return fmt.Errorf("item %q: %w", s, err)
			}
		}
		return nil
	case "int":
		var n int64
		if err := json.Unmarshal(v.Raw, &n); err != nil {
			return fmt.Errorf("expected an integer: %w", err)
		}
		return nil
	case "bool":
		var b bool
		if err := json.Unmarshal(v.Raw, &b); err != nil {
			return fmt.Errorf("expected a bool: %w", err)
		}
		return nil
	case "enum":
		var s string
		if err := json.Unmarshal(v.Raw, &s); err != nil {
			return fmt.Errorf("expected an enum string: %w", err)
		}
		for _, e := range sch.Enum {
			if e.Value == s {
				return nil
			}
		}
		return fmt.Errorf("%q is not a permitted value", s)
	default:
		return fmt.Errorf("unknown preference type %q", sch.Type)
	}
}

func isNull(v apiextv1.JSON) bool {
	return len(v.Raw) == 0 || bytes.Equal(bytes.TrimSpace(v.Raw), []byte("null"))
}

func matchPattern(pattern, s string) error {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("pattern: %w", err)
	}
	if !re.MatchString(s) {
		return fmt.Errorf("%q does not match pattern %q", s, pattern)
	}
	return nil
}
