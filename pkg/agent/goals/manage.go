package goals

import (
	"strings"
	"time"
)

// Management actions a first-party surface may take on a group goal: pause
// is any member's; resume, renew and remove are a manager's (or a platform
// admin's). Resume and renew are approvals — the click is the decision.
const (
	ActPause  = "pause"
	ActResume = "resume"
	ActRenew  = "renew"
	ActRemove = "remove"
)

// Management API operations (ManageRequest.Operation).
const (
	ManageList = "list"
	ManagePlan = "plan"
	ManageAct  = "act"
)

// ManagementHostPrefix starts the session name of a management host. '_' is
// not allowed in a Kubernetes object name, so no AgentSession can ever have
// such a name, and nothing a session does can reach its scope.
const ManagementHostPrefix = "_goal-manage."

// ManagementHost hosts a consent a first-party actor requests from a
// management surface, where there is no session: the consent's request and
// decision records live in its memory scope, exactly as a card's live in the
// session that showed it. One per goal, in the goal's namespace. Nothing is
// ever posted there.
func ManagementHost(g Goal) RenewalHost {
	return RenewalHost{Session: g.Domain.Namespace + "/" + ManagementHostPrefix + g.ID, SessionUID: "goal-manage:" + g.ID}
}

// ManagementHosted reports whether c was requested from a management surface
// (ManagementHost): it is decided there, in the same click, and never posted
// as a card.
func (c *ExecutionConsent) ManagementHosted() bool {
	if c == nil {
		return false
	}
	_, name, ok := strings.Cut(c.Session, "/")
	return ok && strings.HasPrefix(name, ManagementHostPrefix)
}

// consentHost is where a's request for g's consent is hosted: the actor's own
// session, or for the session-less first-party actor, the goal's management
// host.
func consentHost(a Actor, g Goal) RenewalHost {
	if a.FirstParty {
		return ManagementHost(g)
	}
	return RenewalHost{Session: a.Session, SessionUID: a.SessionUID}
}

// BlockedReasonAt derives why g, though it looks active, cannot run at now
// (as Service.Get and List set Goal.BlockedReason).
func BlockedReasonAt(g Goal, now time.Time) string { return blockedReason(g, now) }

// GoalSummary is one group goal as a management surface lists it for a
// viewer. The Can* fields are the viewer's live permissions when listed; each
// action re-checks them when taken.
type GoalSummary struct {
	// Resource names the goal, "agent_goal:<id>", for plan and act.
	Resource string `json:"resource"`
	Title    string `json:"title"`
	// Group is the owning member subject-set, e.g. "<type>:<id>#member"; the
	// surface's kind renders it.
	Group    string `json:"group"`
	State    string `json:"state"`
	Schedule string `json:"schedule,omitempty"`
	// BlockedReason is why an active goal cannot run (BlockedApprovalExpired).
	BlockedReason   string     `json:"blockedReason,omitempty"`
	NextRun         *time.Time `json:"nextRun,omitempty"`
	ApprovalExpires *time.Time `json:"approvalExpires,omitempty"`
	Managers        []string   `json:"managers,omitempty"`
	// NoManagerInGroup: none of the goal's managers is still in its group, so
	// only a platform admin can manage it.
	NoManagerInGroup bool `json:"noManagerInGroup,omitempty"`
	CanPause         bool `json:"canPause,omitempty"`
	CanManage        bool `json:"canManage,omitempty"`
	CanRenew         bool `json:"canRenew,omitempty"`
	// RenewalPending: a renewal of the approval in force awaits a manager.
	RenewalPending bool `json:"renewalPending,omitempty"`
}
