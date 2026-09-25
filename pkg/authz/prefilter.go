package authz

import (
	"regexp"
)

// Prefilter is a cheap regex-based sieve over the user message.
// Returns true when at least one candidate substring matches one of the
// declared entity types' known patterns OR when the type is "unknown"
// (no built-in regex). False means "definitely no entities of any
// declared type" and the caller may skip the LLM call.
//
// Known resourceType patterns:
//   - github_repo, github_repo_url: owner/name in [A-Za-z0-9._-]+/[A-Za-z0-9._-]+
//   - github_org:  bare org-name in [A-Za-z0-9._-]+ (loose; allows fall-through)
//   - linear_team: UUID 8-4-4-4-12 hex OR all-caps short-slug TEAM-NNN form
//   - linear_project: same as team
//
// Unknown types are conservatively allowed (return true if user message
// has any non-whitespace; LLM decides).
//
// Slots whose fillFrom excludes the query/extract source are skipped entirely.
// Nothing downstream can bind them from a user message, so letting one hold the
// sieve open would spend an extractor LLM call — over the user's untrusted text
// — to produce candidates that are discarded.
func Prefilter(userMessage string, types []BoundEntitySpec) bool {
	if userMessage == "" {
		return false
	}
	for _, t := range types {
		if !AllowsExtractedBinding(t.FillFrom) {
			continue
		}
		re := patternFor(t.ResourceType)
		if re == nil {
			// Unknown type: allow the LLM call. We could add a permissive
			// catchall (any non-whitespace word) here, but the cleaner
			// signal to operators is "if you want pre-filter coverage,
			// pick a known resourceType or wait for a future CandidateRegex
			// field on BoundEntityType."
			return true
		}
		if re.MatchString(userMessage) {
			return true
		}
	}
	return false
}

// patternFor returns the regex for a known resource type, or nil.
func patternFor(resourceType string) *regexp.Regexp {
	switch resourceType {
	// Both GitHub repository types take the same pattern, and they must: the
	// sieve reads the USER'S PROSE, where a repository is still spelled
	// owner/name whichever type ends up holding it. github_repo_url is the type
	// the gh toolkit's checks name (its object ids are base64url, never
	// something a human types); github_repo is the forge-keyed type the
	// directory sync writes. Omitting either would not fail — patternFor
	// returns nil and Prefilter conservatively allows the extractor call — it
	// would just quietly stop sieving for the type everyone actually declares.
	case "github_repo", "github_repo_url":
		return reGithubRepo
	case "github_org":
		return reGithubOrg
	case "linear_team", "linear_project":
		return reLinearEntity
	default:
		return nil
	}
}

var (
	// owner/name (allow underscores, dots, hyphens; reject path separators
	// beyond a single slash by anchoring word boundaries).
	reGithubRepo = regexp.MustCompile(`\b[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\b`)

	// Bare org names. Loose match (any word); the LLM filters.
	reGithubOrg = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9._-]{2,}\b`)

	// Linear: UUID or TEAM-XXX style. Anchored on word boundaries.
	reLinearEntity = regexp.MustCompile(
		`\b([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[A-Z]{2,5}-\d+)\b`,
	)
)
