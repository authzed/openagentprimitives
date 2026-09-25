// Package validate enforces the agentskills.io frontmatter constraints plus the
// project's forward-compat rules (no XML tags, no reserved provider words), and
// cross-checks the frontmatter name against the canonical skill directory. Used
// by both the Skill admission webhook (hard deny) and the Skill controller
// (status condition), so the rule set lives in one place.
package validate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

const (
	maxNameLen = 64
	maxDescLen = 1024
)

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	xmlRe    = regexp.MustCompile(`<[^>]+>`)
	reserved = []string{"anthropic", "claude"}
)

// Skill validates a skill's identity + frontmatter + body and returns every
// problem found (not just the first), so the webhook can report them together.
func Skill(canonicalName, fmName, description, body string) []error {
	var errs []error

	n, err := canonical.Parse(canonicalName)
	if err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, validateName(fmName)...)
	errs = append(errs, validateDescription(description)...)
	if strings.TrimSpace(body) == "" {
		errs = append(errs, fmt.Errorf("skill body must not be empty"))
	}
	if err == nil && fmName != n.LastSegment() {
		errs = append(errs, fmt.Errorf("frontmatter name %q must match skill directory %q", fmName, n.LastSegment()))
	}
	return errs
}

// CheckProvenance enforces that a skill lacking confirmed git provenance
// (materialized=false) uses a reserved "local" authority canonical name — so a
// tenant cannot present attacker-controlled content under a trusted
// git-authority name that org-level allowlists would accept. Materialized
// skills keep their git-authority name.
//
// materialized MUST come from an unforgeable signal — a controller owner-ref to
// the SkillSource/ClusterSkillSource that actually produces the name's
// authority, see pkg/tools/skills/materialize. NEVER "spec.Source != nil":
// whoever creates the object sets that field, so a tenant with skills:create
// could spoof a trusted git-authority name for attacker content.
func CheckProvenance(canonicalName string, materialized bool) error {
	n, err := canonical.Parse(canonicalName)
	if err != nil {
		return err
	}
	if !materialized && !n.IsLocal {
		return fmt.Errorf("hand-authored skill must use a local// canonical name; git-authority name %q is reserved for SkillSource-materialized skills", canonicalName)
	}
	return nil
}

func validateName(name string) []error {
	var errs []error
	switch {
	case name == "":
		errs = append(errs, fmt.Errorf("frontmatter name must not be empty"))
	case len(name) > maxNameLen:
		errs = append(errs, fmt.Errorf("frontmatter name %q exceeds %d chars", name, maxNameLen))
	case !nameRe.MatchString(name):
		errs = append(errs, fmt.Errorf("frontmatter name %q must be lowercase kebab-case", name))
	}
	if w, ok := containsReserved(name); ok {
		errs = append(errs, fmt.Errorf("frontmatter name must not contain reserved word %q", w))
	}
	return errs
}

func validateDescription(desc string) []error {
	var errs []error
	switch {
	case strings.TrimSpace(desc) == "":
		errs = append(errs, fmt.Errorf("frontmatter description must not be empty"))
	case len(desc) > maxDescLen:
		errs = append(errs, fmt.Errorf("frontmatter description exceeds %d chars", maxDescLen))
	}
	if xmlRe.MatchString(desc) {
		errs = append(errs, fmt.Errorf("frontmatter description must not contain XML/HTML tags"))
	}
	if w, ok := containsReserved(desc); ok {
		errs = append(errs, fmt.Errorf("frontmatter description must not contain reserved word %q", w))
	}
	return errs
}

func containsReserved(s string) (string, bool) {
	low := strings.ToLower(s)
	for _, w := range reserved {
		if strings.Contains(low, w) {
			return w, true
		}
	}
	return "", false
}
