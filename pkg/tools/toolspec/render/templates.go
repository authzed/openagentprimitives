package render

// Fallback templates used when Generation.Descriptions has no entry for a rule
// path. Match the tone of the LLM-authored descriptions — second-person voice,
// single sentence, terse.

const (
	tmplDenyDestructive    = "This spec hard-denies any destructive operation."
	tmplDenyReads          = "This spec hard-denies reads from these categories."
	tmplDenyWrites         = "This spec hard-denies writes to these categories."
	tmplDenyCredsWrites    = "This spec hard-denies any subcommand that persists credentials."
	tmplAllowNetwork       = "This spec bounds network destinations to the listed hosts."
	tmplAllowFilesystem    = "This spec bounds filesystem writes to these path prefixes."
	tmplAllowCredsRequired = "This spec bounds required credentials to the listed env vars."
)
