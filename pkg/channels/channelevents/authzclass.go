// Cross-channel authz-messaging vocabulary. These types classify a Kind by
// its PRIMARY intent; they are descriptive metadata only — they say WHAT a
// message is, never HOW a channel renders it. See pkg/channels/channelkinds/AUTHZMESSAGING.md.
//
// Audience is the live half: InteractionAudience.Scope is typed on it. The
// rest — Category, Severity, Affordance, AuthzClass, Classify — has no caller
// outside this file today; the semantic interaction model carries tone and
// affordance on the channelinteractions category registry instead.
package channelevents

// Category is what an authz message is for.
type Category string

const (
	CategoryDecisionRequest Category = "decision_request" // approve/deny gate
	CategoryActionRequest   Category = "action_request"   // non-binary call-to-action
	CategoryPendingStatus   Category = "pending_status"   // awaiting / in-progress
	CategoryDecisionOutcome Category = "decision_outcome" // approved/denied/timeout
	CategoryNotice          Category = "notice"           // FYI, no action
)

// Severity is the cross-cutting tone. "warnings" live here (e.g. the
// userPassthrough elevated-risk framing is a warning on a decision_request).
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityDanger  Severity = "danger"
)

// Audience is who should see a message. Visibility is gated by AuthzClass.Sensitive.
type Audience string

const (
	AudienceApprovers    Audience = "approvers"     // resolved from a SpiceDB subject
	AudienceRequester    Audience = "requester"     // the person who triggered the gate
	AudienceParticipants Audience = "participants"  // thread / public
	AudienceSpecificUser Audience = "specific_user" // a named user (e.g. started_by)
)

// Affordance is the interaction surface. The decision return path
// (block_action vs response_url) is a channel detail, not part of this vocab.
type Affordance string

const (
	AffordanceNone    Affordance = "none"
	AffordanceButtons Affordance = "buttons"
	AffordanceModal   Affordance = "modal"
	AffordanceLink    Affordance = "link"
)

// AuthzClass is the primary classification of a Kind.
type AuthzClass struct {
	Category        Category
	DefaultAudience Audience
	Severity        Severity
	// Sensitive, when true, means this message MUST stay private — a channel
	// may never route it to AudienceParticipants.
	Sensitive bool
}

// classTable maps each authz Kind to its primary classification. Non-authz
// kinds are absent (Classify returns ok=false for them).
var classTable = map[Kind]AuthzClass{
	KindPermissionRequest: {CategoryDecisionRequest, AudienceSpecificUser, SeverityInfo, false},
}

// Classify returns the primary classification of an authz Kind. ok is false
// for non-authz kinds (general messaging, streaming, tool-session, etc.).
//
// A Kind classifies by its PRIMARY intent; a flow may still emit secondary
// messages of other categories (e.g. a decision_request flow also emitting a
// pending_status to participants). That richer behavior lives in the
// per-channel descriptor, not here.
func Classify(k Kind) (AuthzClass, bool) {
	c, ok := classTable[k]
	return c, ok
}
