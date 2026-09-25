// Package userprofile owns the kind-neutral user-profile vocabulary: which
// fields exist, which are exposed by default, and the single Filter that
// decides what an agent may see. Kept dependency-free (stdlib only) so both
// the channel kinds that produce profiles and the runner that renders them
// can import it without a cycle.
//
// One Filter in the whole codebase, deliberately: the capability's config is
// the ONLY thing that decides exposure, and a second filtering site would be
// free to drift from it.
package userprofile

import "fmt"

// Field is a kind-neutral profile field key. The set is closed — a channel
// kind maps its native fields onto these, never the reverse.
type Field string

const (
	FieldDisplayName Field = "displayName"
	FieldRealName    Field = "realName"
	FieldEmail       Field = "email"
	FieldTitle       Field = "title"
	FieldPronouns    Field = "pronouns"
	FieldTimezone    Field = "timezone"
	FieldLocale      Field = "locale"
	FieldStatusText  Field = "statusText"
	FieldStartDate   Field = "startDate"
	FieldAccountType Field = "accountType"
	FieldPhone       Field = "phone"
)

// Profile is a kind-neutral user profile. Every field is optional; a kind
// populates what it can and leaves the rest zero. All string content here is
// USER-AUTHORED and therefore untrusted — see Render.
type Profile struct {
	DisplayName string
	RealName    string
	Email       string
	Title       string
	Pronouns    string
	// Timezone is the IANA name; TimezoneLabel is the human rendering. They
	// travel together under FieldTimezone — exposing an offset without its
	// name (or vice versa) is never useful.
	Timezone      string
	TimezoneLabel string
	Locale        string
	StatusText    string
	StartDate     string
	// AccountType is a coarse standing label ("member", "guest", "admin"),
	// derived by the kind. It is descriptive ONLY and is never an
	// authorization input.
	AccountType string
	Phone       string
}

// ordered is the canonical field order: declaration order drives both
// AllFields and the rendered block, so the agent sees a stable layout.
var ordered = []Field{
	FieldDisplayName, FieldRealName, FieldEmail, FieldTitle, FieldPronouns,
	FieldTimezone, FieldLocale, FieldStatusText, FieldStartDate,
	FieldAccountType, FieldPhone,
}

// AllFields returns every known field, in canonical order.
func AllFields() []Field {
	out := make([]Field, len(ordered))
	copy(out, ordered)
	return out
}

// DefaultFields is what an empty `fields` list resolves to.
//
// Deliberately NOT "everything", which is how the sibling artifacts
// capability treats an empty `renderers` list. Personal data earns a
// conservative default: an operator who writes `user_profile: {}` gets the
// three fields that serve the tailoring use case, not an email and a phone
// number they never asked to expose.
func DefaultFields() []Field {
	return []Field{FieldDisplayName, FieldTitle, FieldTimezone}
}

// known indexes ordered for ParseFields.
var known = func() map[Field]struct{} {
	m := make(map[Field]struct{}, len(ordered))
	for _, f := range ordered {
		m[f] = struct{}{}
	}
	return m
}()

// ParseFields converts operator-supplied strings to Fields, preserving their
// order. An empty list yields DefaultFields.
//
// An unknown or duplicated name is an ERROR, not a skip: silently dropping it
// would hand the operator a narrower profile than they configured with no
// signal anywhere (AGENTS.md: never silently drop errors).
func ParseFields(names []string) ([]Field, error) {
	if len(names) == 0 {
		return DefaultFields(), nil
	}
	out := make([]Field, 0, len(names))
	seen := make(map[Field]struct{}, len(names))
	for _, n := range names {
		f := Field(n)
		if _, ok := known[f]; !ok {
			return nil, fmt.Errorf("unknown profile field %q", n)
		}
		if _, dup := seen[f]; dup {
			return nil, fmt.Errorf("duplicate profile field %q", n)
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out, nil
}
