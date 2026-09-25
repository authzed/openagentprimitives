package userprofile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fullProfile() Profile {
	return Profile{
		DisplayName:   "Dana Whitfield",
		RealName:      "Dana Whitfield",
		Email:         "dana@example.com",
		Title:         "Director of Support",
		Pronouns:      "they/them",
		Timezone:      "America/New_York",
		TimezoneLabel: "Eastern Standard Time",
		Locale:        "en-US",
		StatusText:    "In a meeting",
		StartDate:     "2021-03-01",
		AccountType:   "member",
		Phone:         "+1-555-0100",
	}
}

func TestFilterKeepsOnlyAllowlistedFields(t *testing.T) {
	cases := []struct {
		name  string
		allow []Field
		want  Profile
	}{
		{
			name:  "empty allowlist yields the zero profile, never the full one",
			allow: nil,
			want:  Profile{},
		},
		{
			name:  "single field: only title survives",
			allow: []Field{FieldTitle},
			want:  Profile{Title: "Director of Support"},
		},
		{
			name:  "timezone carries its label together, never split",
			allow: []Field{FieldTimezone},
			want:  Profile{Timezone: "America/New_York", TimezoneLabel: "Eastern Standard Time"},
		},
		{
			name:  "default set: displayName, title, timezone",
			allow: DefaultFields(),
			want: Profile{
				DisplayName:   "Dana Whitfield",
				Title:         "Director of Support",
				Timezone:      "America/New_York",
				TimezoneLabel: "Eastern Standard Time",
			},
		},
		{
			name:  "all fields round-trips the input unchanged",
			allow: AllFields(),
			want:  fullProfile(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Filter(fullProfile(), tc.allow))
		})
	}
}

// TestAllFieldsAreWiredIntoLabelsValueOfAndFilter guards against the three
// independent enumerations of Field (Filter's switch, render.go's labels
// map, render.go's valueOf switch) drifting apart. Nothing forces them to
// stay in sync — a field added to `ordered` and missed in valueOf would
// silently never render, and one missed in labels would render with an
// empty label — so this walks AllFields() and checks all three sites for
// every field.
func TestAllFieldsAreWiredIntoLabelsValueOfAndFilter(t *testing.T) {
	full := fullProfile()
	for _, f := range AllFields() {
		t.Run(string(f)+": has a label, a rendered value, and survives Filter when allowlisted alone", func(t *testing.T) {
			assert.NotEmpty(t, labels[f], "a field missing from the labels map renders with an empty label")
			assert.NotEmpty(t, valueOf(full, f), "a field missing from valueOf's switch silently never renders")

			filtered := Filter(full, []Field{f})
			assert.NotEmpty(t, valueOf(filtered, f), "a field missing from Filter's switch is silently zeroed even when allowlisted")
		})
	}
}

func TestParseFields(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []Field
		wantErr string
	}{
		{name: "empty input yields the default set", in: nil, want: DefaultFields()},
		{name: "known names parse in order", in: []string{"pronouns", "title"}, want: []Field{FieldPronouns, FieldTitle}},
		{name: "unknown name is rejected, never ignored", in: []string{"title", "salary"}, wantErr: `unknown profile field "salary"`},
		{name: "duplicate name is rejected", in: []string{"title", "title"}, wantErr: `duplicate profile field "title"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFields(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err, "an unusable field list must surface, never be silently narrowed")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
