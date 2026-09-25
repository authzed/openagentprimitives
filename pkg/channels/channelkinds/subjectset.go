package channelkinds

import (
	"fmt"
	"strings"
)

// SubjectSetDescription is a channel kind's human-readable rendering of a
// SpiceDB subject-set ref.
type SubjectSetDescription struct {
	// Label is what the channel calls this population, in its own words —
	// "#eng", "this thread". Empty when the kind cannot name it.
	Label string
	// Size is the population count. Zero means UNKNOWN, not empty: a card must
	// never fabricate a count, so an unknown size is rendered by omitting it
	// rather than by printing "0 people".
	Size int
}

// SubjectSetDescriber is an optional Kind capability: render a SpiceDB
// subject-set ref into something a human can act on.
//
// Only the kind knows what a ref means — `slack_channel:C0421#member` is `#eng`
// to Slack and an opaque string to a generic renderer. Follows the same
// contract as AudienceResolver and OwnerGroupProvider: consumers type-assert,
// and a kind that does not implement it degrades to a plainer answer, never to
// a wrong one.
//
// Returning ok=false is always safe. A kind that cannot describe a particular
// ref — or cannot reach its API, or lacks the scope to ask — should return
// false rather than guess. Slack does not implement this interface at all
// today, and even once it does, false will remain the common answer:
// conversations.info and conversations.members need channels:read and
// groups:read, which the generated app manifest requests only when the
// opt-in directory_sync capability is granted
// (pkg/channels/channelkinds/slack/features.go) — a scope pair most agents'
// apps will not carry, since it exists for a RelationshipSource's token, not
// for describing a subject set.
type SubjectSetDescriber interface {
	DescribeSubjectSet(subjectSetRef string) (SubjectSetDescription, bool)
}

// ApproverPopulation is everything known about who may approve a request.
type ApproverPopulation struct {
	// Kind is the channel kind, type-asserted for SubjectSetDescriber. Untyped
	// so this stays usable from packages that do not depend on the Kind
	// interface; a value that does not implement the describer is fine.
	Kind any

	// Ref is the SpiceDB subject-set ref the population came from, if any.
	Ref string

	// Names are resolved individual approvers, when few enough to have been
	// worth resolving.
	Names []string

	// Size is the total population. Zero means unknown.
	Size int

	// MaxNamedApprovers is the largest population rendered by name.
	MaxNamedApprovers int
}

// DescribeApprovers renders an approver population for an approval card.
//
// The rule is not "always enumerate": a 200-member channel listed on a card is
// worse than useless, and the enumeration itself costs a lookup. It scales with
// size and defers naming to the channel, degrading through four levels:
//
//	dev@example.com                                  one, or few enough to name
//	anyone in #eng (23 people)                       the kind described it fully
//	anyone in #eng                                   labelled, size unknown
//	anyone with approve on this session (23 people)  sized, unlabelled
//	anyone with approve on this session              the honest floor
//
// The floor is correct on its own terms rather than a placeholder — it is what
// Slack renders today — so a missing describer degrades legibility and never
// blocks an approval.
func DescribeApprovers(p ApproverPopulation) string {
	// Few enough to name: the most useful rendering, so it wins.
	if len(p.Names) > 0 && p.MaxNamedApprovers > 0 && len(p.Names) <= p.MaxNamedApprovers {
		return strings.Join(p.Names, ", ")
	}

	label := ""
	size := p.Size
	if d, ok := p.Kind.(SubjectSetDescriber); ok && p.Ref != "" {
		if desc, ok := d.DescribeSubjectSet(p.Ref); ok {
			label = desc.Label
			if desc.Size > 0 {
				size = desc.Size
			}
		}
	}

	if label != "" {
		if size > 0 {
			return fmt.Sprintf("anyone in %s (%d people)", label, size)
		}
		return "anyone in " + label
	}
	if size > 0 {
		return fmt.Sprintf("anyone with approve on this session (%d people)", size)
	}
	return "anyone with approve on this session"
}
