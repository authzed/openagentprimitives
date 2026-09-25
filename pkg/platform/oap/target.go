package oap

import (
	"fmt"
	"strings"
)

// Target identifies a field inside one CR for a binding overlay. Syntax:
//
//	<Kind>/<name>#<segment>(.<segment>)*
//
// A segment is a map key ("spec", "channelID") or a typed list selector
// "field[key]" selecting the list element whose discriminator equals key
// (e.g. "boundEntities[github_repo]" → the element with
// resourceType=="github_repo"). See discriminatorFor in overlay.go.
type Target struct {
	Kind string
	Name string
	Path []Segment
}

type Segment struct {
	Field    string // always set
	Selector string // non-empty → list element selected by discriminator==Selector
	Append   bool   // "field[]" (empty brackets) → union the answer list onto the field
}

func ParseTarget(s string) (Target, error) {
	slash := strings.IndexByte(s, '/')
	hash := strings.IndexByte(s, '#')
	if slash < 0 || hash < 0 || hash < slash {
		return Target{}, fmt.Errorf("target %q: want Kind/name#path", s)
	}
	t := Target{Kind: s[:slash], Name: s[slash+1 : hash]}
	if t.Kind == "" || t.Name == "" {
		return Target{}, fmt.Errorf("target %q: empty kind or name", s)
	}
	for _, raw := range strings.Split(s[hash+1:], ".") {
		if raw == "" {
			return Target{}, fmt.Errorf("target %q: empty path segment", s)
		}
		seg := Segment{Field: raw}
		if open := strings.IndexByte(raw, '['); open >= 0 {
			if !strings.HasSuffix(raw, "]") {
				return Target{}, fmt.Errorf("target %q: unterminated selector in %q", s, raw)
			}
			seg.Field = raw[:open]
			seg.Selector = raw[open+1 : len(raw)-1]
			if seg.Field == "" {
				return Target{}, fmt.Errorf("target %q: bad selector in %q", s, raw)
			}
			if seg.Selector == "" {
				// Empty brackets "field[]" are the append/union marker, not an
				// element selector. The answer list is merged onto whatever the
				// CR already declares for this field (see setAtPath).
				seg.Append = true
			}
		}
		t.Path = append(t.Path, seg)
	}
	for i, seg := range t.Path {
		if seg.Append && i != len(t.Path)-1 {
			return Target{}, fmt.Errorf("target %q: append marker %q[] must be the final segment", s, seg.Field)
		}
	}
	return t, nil
}
