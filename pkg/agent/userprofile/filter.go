package userprofile

// Filter returns a copy of p carrying ONLY the allowlisted fields. Anything
// not named is zeroed.
//
// allow must hold only known Fields: every caller builds it via ParseFields,
// AllFields, or intersectFields, all validated against `known`. An unrecognised
// Field is silently ignored (no default case), which is safe only because
// nothing constructs allow by hand.
//
// Allowlist, never blocklist: a field added to Profile stays invisible until it
// is added to `ordered` AND an operator names it. Copy-everything-then-delete
// would leak every new field by default.
func Filter(p Profile, allow []Field) Profile {
	var out Profile
	for _, f := range allow {
		switch f {
		case FieldDisplayName:
			out.DisplayName = p.DisplayName
		case FieldRealName:
			out.RealName = p.RealName
		case FieldEmail:
			out.Email = p.Email
		case FieldTitle:
			out.Title = p.Title
		case FieldPronouns:
			out.Pronouns = p.Pronouns
		case FieldTimezone:
			out.Timezone = p.Timezone
			out.TimezoneLabel = p.TimezoneLabel
		case FieldLocale:
			out.Locale = p.Locale
		case FieldStatusText:
			out.StatusText = p.StatusText
		case FieldStartDate:
			out.StartDate = p.StartDate
		case FieldAccountType:
			out.AccountType = p.AccountType
		case FieldPhone:
			out.Phone = p.Phone
		}
	}
	return out
}
