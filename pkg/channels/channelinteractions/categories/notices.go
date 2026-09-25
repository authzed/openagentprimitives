// Registered NOTICE categories: the zero-action, one-way messages the system
// sends users — errors, denials, degradation, lifecycle events.
//
// This file is the single place a notice's TONE is decided. A publisher
// supplies copy (lead, body, what to do about it); it cannot pick a tone, a
// colour, or a glyph. That split is what makes tone reviewable: every message
// the system can send is one file, read top to bottom, instead of a judgement
// made independently at fifty call sites.
//
// When adding a row, ask what a reader can DO about the message — that answer
// is the tone, and for the tones Tone.RequiresNextStep names it must also
// become a NextStep at every publish site.
package categories

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

const (
	// InternalError is the generic "we broke, not you" notice for a failure the
	// user can only respond to by retrying. Shared by every inbound path that
	// fails before a message reaches the agent.
	//
	// Degraded rather than critical: the session survives and a retry is a real
	// remedy. Critical is reserved for messages after which retrying is futile.
	InternalError = "internal_error"

	// PortalLinkFailed — a portal/account-settings link could not be minted.
	// Degraded: the user typed a trigger phrase and is waiting, but a retry is
	// a real remedy.
	PortalLinkFailed = "portal_link_failed"

	// SessionEnded — a reply arrived for a session that failed unrecoverably.
	// Critical, not degraded: replying again in this thread can only re-refuse,
	// so the only way forward is a new thread.
	SessionEnded = "session_ended"

	// SessionContinuedInherited — a finished conversation is being picked up in
	// a fresh session, same thread, history carried over. Housekeeping: nothing
	// was lost and nothing is required.
	SessionContinuedInherited = "session_continued_inherited"

	// ThreadTakeoverInherited — a DIFFERENT user is continuing a terminal
	// thread, with prior history carried over. Housekeeping for the same reason.
	ThreadTakeoverInherited = "thread_takeover_inherited"

	// ThreadTakeoverHalted — a different user is continuing a thread whose
	// previous session was HALTED, so no history carries over. Degraded rather
	// than housekeeping: context the reader may assume is present is not.
	//
	// A separate row from ThreadTakeoverInherited because they are separate
	// events. They are one branch apart on the publisher's side, but one is
	// routine and the other silently drops context, and a shared row would
	// force them to share a tone.
	ThreadTakeoverHalted = "thread_takeover_halted"

	// ArchivedSessionNotYours — someone tried to revive an archived session
	// they did not start. Degraded: nothing is broken, but they cannot have
	// what they asked for and must start their own thread.
	ArchivedSessionNotYours = "archived_session_not_yours"

	// SessionOwnerRecordMissing — the session's ownership record has not been
	// written yet, so the authorization check does not recognise its own
	// starter. Degraded: usually a startup race that a retry clears.
	//
	// It must NOT read as a permission decision — nobody denied anyone anything
	// — which is why it is its own category rather than sharing one with
	// ArchivedSessionNotYours.
	SessionOwnerRecordMissing = "session_owner_record_missing"

	// JoinNoApprover — a join request arrived for a session with no human owner
	// (cron-spawned), so there is nobody who could approve it. Critical: a
	// permanent configuration condition, not a transient one, so retrying will
	// never help.
	JoinNoApprover = "join_no_approver"

	// StartNoAdmin — an org non-member tried to start a session and no
	// platform admin is resolvable to approve the start_approval request.
	// The start-gate sibling of JoinNoApprover, and critical for the same
	// reason: a permanent configuration condition (no admin granted), not a
	// transient one.
	StartNoAdmin = "start_no_admin"

	// StartAwaitingApproval — someone messaged a thread whose session is
	// parked awaiting a platform admin's start approval. Informational and
	// non-terminal: the approval may still land.
	StartAwaitingApproval = "start_awaiting_approval"

	// AgentFailed — the session hit an unrecoverable fault and stopped.
	//
	// It reaches the user as a notice rather than as agent speech: published as
	// a chat message it would render in the agent's own bubble, reading as the
	// agent calmly reporting its own death. The system talking ABOUT the agent
	// has to look different from the agent talking.
	AgentFailed = "agent_failed"

	// AgentStalled — no forward progress for long enough that the watchdog
	// gave up waiting. Degraded rather than critical: the diagnosis is a
	// guess, and re-sending genuinely does resolve most of these.
	AgentStalled = "agent_stalled"

	// ContinuationDenied — a request to continue a conversation in a fresh
	// session was refused by the authorization gate.
	ContinuationDenied = "continuation_denied"

	// SubagentNotAddressable — a person tried to open a turn into a delegated
	// agent that was handed a bounded piece of work and takes direction only
	// from the agent that handed it over. Not an authorization refusal: nobody
	// has standing to drive one of these, and the reader talks to the
	// delegating agent instead.
	SubagentNotAddressable = "subagent_not_addressable"

	// SessionCost — what the session cost. Carries GlyphMoney: neither its
	// tone nor its finality is what the reader came for, which is the narrow
	// case a glyph override is for.
	SessionCost = "session_cost"

	// ForkDenied — a restart-from-here was refused because the requester does
	// not own the session.
	ForkDenied = "fork_denied"

	// SessionHalted — a gate stopped the session before it could run. Critical
	// and terminal: the run is over and this session will not resume.
	SessionHalted = "session_halted"

	// ToolboxUnavailable — a sidecar toolset the agent was told about failed
	// to come up, so those tools are gone for the whole session. Degraded
	// rather than critical: the agent can still work with what remains.
	ToolboxUnavailable = "toolbox_unavailable"

	// ToolAuthzDisabled — this agent class runs every tool call without a
	// permission check. Not an error, and nothing is broken; it is a standing
	// property of how the session is configured that a participant should
	// know about. Privacy tone: it is about what can reach whose data.
	ToolAuthzDisabled = "tool_authz_disabled"

	// ThreadContinuedFrom is the first message of a forked thread, naming the
	// conversation it came from so a reader arriving cold has the context.
	// The thread it heads is live, so it is not terminal.
	ThreadContinuedFrom = "thread_continued_from"

	// ThreadMoved is posted in the OLD thread when a session restarts into a
	// new one. Terminal — nothing further happens in the thread the reader is
	// looking at — while the body points at where the conversation went.
	ThreadMoved = "thread_moved"

	// AgentUnavailable — a message arrived for an agent that cannot currently
	// run: one of the services it signs in to needs re-authorizing, or some
	// other part of its setup stopped resolving. No session is started, because
	// a session of an unhealthy agent could only stall before its first turn.
	//
	// Degraded: nothing the reader did is wrong, nothing is permanently lost,
	// and the same message sent again once an administrator has fixed the agent
	// will work. That is also why the thread is not terminal.
	AgentUnavailable = "agent_unavailable"

	// RevocationNotApplied — somebody withdrew access this session is using and
	// the withdrawal could not be applied here, so the capability may still be
	// live inside the running session.
	//
	// Privacy, and the distinction is the whole reason it is its own row.
	// Degraded would promise that trying again is the remedy, but re-sending a
	// message withdraws nothing. Critical is for hostile input or a dead
	// session, and it is neither. What is being reported is who can still reach
	// whose data — the question privacy is for.
	//
	// Not terminal: the reader can still act, and TonePrivacy makes saying so
	// mandatory (Tone.RequiresNextStep). End the conversation, or withdraw the
	// access where it was originally granted.
	RevocationNotApplied = "revocation_not_applied"

	// AttachmentReadFailed — one or more of a message's inbound attachments were
	// not read: the gate was closed, the type is unsupported, a limit was
	// exceeded, or a fetch/upload/extraction step failed transiently. Degraded:
	// the session survives, and for the transient case resending is a real
	// remedy. Human-facing sibling of the bracketed note channelsd writes into
	// the turn for the agent — an agent that judges the failure unimportant must
	// not be the only one who knows about it.
	AttachmentReadFailed = "attachment_read_failed"

	// AttachmentNotEnabled — a message carried files and the gate was closed:
	// the Channel opt-in is off, the AgentClass lacks the capability, or the
	// bound kind cannot fetch at all. NOTHING WAS ATTEMPTED, which is exactly
	// why this is not AttachmentReadFailed: no fetch ran, so nothing failed,
	// and "try sending the file again" is advice that cannot work. Unavailable
	// rather than degraded because only someone with configuration access can
	// change the answer.
	AttachmentNotEnabled = "attachment_not_enabled"

	// AttachmentUnreadable — the gate was OPEN and the file was refused on its
	// own merits: no extractor claims the type, it exceeds a size ceiling, or
	// it fell past the per-message count. Permanent for this file, and the
	// remedy belongs to the user (another format, a smaller file, fewer
	// files), never to an administrator — which is the whole reason it is held
	// apart from AttachmentNotEnabled despite sharing a tone.
	//
	// Carries GlyphWarning so it reads louder than the quiet tones without
	// claiming something broke.
	AttachmentUnreadable = "attachment_unreadable"

	// CompletionRequirementBypassed — the agent finished a round while
	// something its configuration says every round must produce was missing,
	// and said why.
	//
	// A notice rather than a prompt: the decision is already taken and the
	// round is over, so there is nothing left to approve. What the reader gets
	// is the fact plus the agent's stated reason — the whole point of allowing
	// the override at all is that a person learns it happened.
	//
	// Degraded: the reader ends up with less than the agent was set up to give
	// them, nothing is broken, and asking again is a real remedy. Which also
	// makes a NextStep mandatory (Tone.RequiresNextStep), and that is correct
	// here — being told what was skipped without being told one can ask for it
	// is the shape of a notice a reader can do nothing with.
	CompletionRequirementBypassed = "completion_requirement_bypassed"
)

// registerNotices registers every notice category. Kept out of init so a test
// that Resets the process-wide registry can put it back: an init runs once per
// process and cannot be re-invoked.
func registerNotices() {
	notices := []channelinteractions.Category{
		// Retryable: the message never reached the agent, but sending it again
		// is a real remedy, so this is not terminal.
		{Name: InternalError, Tone: channelinteractions.ToneDegraded},
		{Name: PortalLinkFailed, Tone: channelinteractions.ToneDegraded},
		{Name: SessionOwnerRecordMissing, Tone: channelinteractions.ToneDegraded},

		// Terminal: nothing the reader does in this thread will change the
		// outcome. Squares are rare on purpose — that is what makes them read
		// as "stop waiting" rather than as one more alarm.
		{Name: SessionEnded, Tone: channelinteractions.ToneCritical, Terminal: true},
		{Name: JoinNoApprover, Tone: channelinteractions.ToneCritical, Terminal: true},
		{Name: StartNoAdmin, Tone: channelinteractions.ToneCritical, Terminal: true},
		{Name: AgentFailed, Tone: channelinteractions.ToneCritical, Terminal: true},

		// Stalled is NOT terminal, and the distinction is the useful part: the
		// watchdog is reporting an absence of progress, not a confirmed death,
		// and the session may still be alive behind it.
		{Name: AgentStalled, Tone: channelinteractions.ToneDegraded},
		// Waiting, not degraded: a parked start approval is the system
		// working as configured, and the reader's move is patience.
		{Name: StartAwaitingApproval, Tone: channelinteractions.ToneWaiting},
		{Name: ContinuationDenied, Tone: channelinteractions.ToneDegraded},
		{Name: ForkDenied, Tone: channelinteractions.ToneDegraded},
		{Name: ToolboxUnavailable, Tone: channelinteractions.ToneDegraded},
		{Name: AgentUnavailable, Tone: channelinteractions.ToneDegraded},
		{Name: SessionHalted, Tone: channelinteractions.ToneCritical, Terminal: true},
		{Name: ToolAuthzDisabled, Tone: channelinteractions.TonePrivacy},

		// Housekeeping with a glyph: a cost report is neither urgent nor final,
		// and a reader scanning for money wants the money symbol.
		{Name: SessionCost, Tone: channelinteractions.ToneHousekeeping,
			Glyph: channelinteractions.GlyphMoney},

		{Name: ThreadContinuedFrom, Tone: channelinteractions.ToneHousekeeping},
		// Terminal from the reader's position: the thread they are looking at
		// has nothing more coming, even though the conversation continues
		// elsewhere. That is what stops someone waiting in a dead thread.
		{Name: ThreadMoved, Tone: channelinteractions.ToneHousekeeping, Terminal: true},

		// Refused, but with somewhere to go: starting a fresh thread gets the
		// reader what they wanted, so this is degraded rather than critical
		// and stays a circle.
		{Name: ArchivedSessionNotYours, Tone: channelinteractions.ToneDegraded},
		// Same shape, one level down the delegation tree: the message was
		// refused, the agent is fine, and addressing the agent that delegated
		// the work gets the reader what they wanted.
		{Name: SubagentNotAddressable, Tone: channelinteractions.ToneDegraded},

		// Data already reached someone who could not otherwise see it. Privacy
		// rather than degraded: nothing is broken, and how much it matters
		// depends on whose data it is — which the reader knows and the system
		// does not. Not terminal: the reader can still review who has access
		// and revoke the connection.
		{Name: InfoLeakageNotice, Tone: channelinteractions.TonePrivacy},
		// The mirror image of the row above — data that may STILL reach someone
		// who was supposed to stop being able to see it. Same tone, and
		// non-terminal for the same reason.
		{Name: RevocationNotApplied, Tone: channelinteractions.TonePrivacy},

		// Lifecycle churn. The thread carries on either way; the reader is
		// being kept informed, not asked for anything.
		{Name: SessionContinuedInherited, Tone: channelinteractions.ToneHousekeeping},
		{Name: ThreadTakeoverInherited, Tone: channelinteractions.ToneHousekeeping},
		// …except this one, where history did NOT carry over. Degraded rather
		// than housekeeping because a reader who assumes the earlier context
		// is present will be wrong.
		{Name: ThreadTakeoverHalted, Tone: channelinteractions.ToneDegraded},

		// Three rows, not one: Tone is a fixed property of a category by
		// design, so the previous single row made every attachment problem
		// orange — including a capability that was simply switched off, where
		// retrying is wrong. The remedies differ, so the categories differ.
		{Name: AttachmentNotEnabled, Tone: channelinteractions.ToneUnavailable},
		{Name: AttachmentUnreadable, Tone: channelinteractions.ToneUnavailable,
			Glyph: channelinteractions.GlyphWarning},
		{Name: AttachmentReadFailed, Tone: channelinteractions.ToneDegraded},

		// The reader was promised something the round did not deliver. Degraded
		// rather than housekeeping precisely because a reader who assumes they
		// got the whole result will be wrong.
		{Name: CompletionRequirementBypassed, Tone: channelinteractions.ToneDegraded},
	}
	for _, c := range notices {
		c.Notice = true
		c.Resurface = channelinteractions.ResurfaceNone
		channelinteractions.Register(c)
	}
}
