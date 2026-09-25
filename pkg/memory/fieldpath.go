package memory

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ContentKeys returns the top-level JSON object keys a Kind's Content struct
// serializes to — the vocabulary FieldFilter.Path draws from. `json:"-"` fields
// are omitted; an untagged exported field contributes its Go name, matching what
// encoding/json emits. An embedded struct with no JSON name is flattened as
// encoding/json flattens it; one with a name contributes only that name, since
// it serializes as a nested object.
//
// nil for a nil or non-struct type, meaning "no vocabulary is knowable" —
// callers must NOT read it as "this Kind has no keys".
func ContentKeys(t reflect.Type) map[string]struct{} {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	out := map[string]struct{}{}
	collectContentKeys(t, out, map[reflect.Type]struct{}{})
	return out
}

func collectContentKeys(t reflect.Type, out map[string]struct{}, seen map[reflect.Type]struct{}) {
	if _, dup := seen[t]; dup {
		// A struct that embeds its own type (directly or through a cycle):
		// its keys are already recorded, and recursing would not terminate.
		return
	}
	seen[t] = struct{}{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectContentKeys(ft, out, seen)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = struct{}{}
	}
}

// validateFieldPaths rejects a FieldEquals predicate whose Path is not a content
// key of any Kind the query names.
//
// An unresolvable path is INVISIBLE otherwise: backends render a predicate as
// content->>'<path>', so a path naming no stored key extracts NULL, every
// comparison is false, and the query answers empty forever, with no error and no
// log. The caller cannot tell "nothing matched" from "you asked a question that
// can never match" — two accessors shipped with the Go field name in Path and
// returned empty on both SQL backends for exactly that reason.
//
// It returns an ERROR rather than a QueryResult.DroppedPredicates entry: that
// channel reports backend capability degradation, which widens the answer and
// which callers are documented not to branch on, whereas an unresolvable path
// narrows the answer to nothing and always means the code is wrong.
//
// Deliberately FAIL-OPEN where it cannot know: with no Kinds named there is no
// schema to resolve against, and one unregistered or schema-less Kind among them
// means some entries could legally carry any key at all.
func validateFieldPaths(q Query) error {
	if len(q.FieldEquals) == 0 || len(q.Kinds) == 0 {
		return nil
	}
	known := map[string]struct{}{}
	for _, name := range q.Kinds {
		k, ok := LookupKind(name)
		if !ok {
			return nil
		}
		keys := ContentKeys(k.ContentSchema())
		if keys == nil {
			return nil
		}
		for key := range keys {
			known[key] = struct{}{}
		}
	}
	for _, ff := range q.FieldEquals {
		if _, ok := known[ff.Path]; ok {
			continue
		}
		// Wraps ErrInvalidQuery so httpsrv answers 400: this blames the
		// CALLER, and httpclient retries 5xx eight times over ~17s.
		return fmt.Errorf(
			"%w: FieldEquals path %q is not a content key of kind(s) %s%s; "+
				"a path is the key the entry is STORED under (the `json` tag), not the Go field name, "+
				"and is a top-level key — known keys: %s",
			ErrInvalidQuery, ff.Path, strings.Join(q.Kinds, ", "), suggestKey(ff.Path, known), sortedKeys(known))
	}
	return nil
}

// suggestKey returns a " (did you mean \"x\"?)" clause when a known key differs
// from path only by case — the Go-field-name / json-tag slip this catches.
func suggestKey(path string, known map[string]struct{}) string {
	for _, k := range sortedKeys(known) {
		if strings.EqualFold(k, path) {
			return fmt.Sprintf(" (did you mean %q?)", k)
		}
	}
	return ""
}

func sortedKeys(known map[string]struct{}) []string {
	out := make([]string, 0, len(known))
	for k := range known {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
