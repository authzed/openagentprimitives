package chatcmd

// tui.go renders the `oap agent chat` full-screen TUI: a
// scrolling timeline of you/agent messages and inline collapsible tool
// sessions, a status line, an always-visible input box, a plan overlay,
// and a blocking generic-interaction decision modal. The local-kind senders
// feed it typed render events; it feeds typed input + interaction decisions
// back to the local Listener. All layout/rendering lives here — the senders
// are thin translators.
//
// What it takes from pkg/cli/tui, and what it does not:
//
//   - Every color, every speaker marker and the timeline/overlay frame come
//     from tui.Theme, so a chat looks like a wizard and a list command.
//   - The header obeys tui.Truncate at the terminal's live width — Chrome's
//     rule that no rendered line may overflow. Here it is load-bearing rather
//     than cosmetic: a header that wrapped would push every pane down one row
//     and desync the heights layout() computed.
//   - tui.Chrome itself is NOT used. It takes its width from Theme.Caps,
//     measured once when the run is built, and offers no way to be told a new
//     one; this model owns the alternate screen and re-lays-out on every
//     tea.WindowSizeMsg. Chrome also places a body as one block, while this
//     View stacks five regions whose heights it budgets itself, and its step
//     rail has no analogue: a chat is not a forward-only sequence of screens.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	local "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// --- TUI-internal message types (not from the local kind) ---

// msgLocalEcho echoes the user's just-submitted input into the timeline
// immediately, before the runner round-trips. Emitted by the input
// handler.
type msgLocalEcho struct{ Text string }

// msgLiveViewUnavailable is emitted ONCE at session startup when the
// artifact view-link minter could not be built (signing key or webd URL
// absent). Renders as a dim informational timeline note so the user is
// not surprised when no "View in browser:" lines appear.
type msgLiveViewUnavailable struct{ Reason string }

// msgStartupNotice is a dim informational timeline note emitted at session
// startup for a degraded-but-not-fatal condition that is NOT about browser
// view links (which msgLiveViewUnavailable owns, and whose renderer bakes that
// wording in). Its Text is rendered verbatim, so the sender says what actually
// happened rather than borrowing an unrelated sentence.
type msgStartupNotice struct{ Text string }

// msgSessionPhase carries an AgentSession phase transition observed by
// the lifecycle watcher goroutine. FailureReason / FailureMessage are
// populated from the AgentSession's Failed condition when Phase=Failed.
type msgSessionPhase struct {
	Phase          string
	FailureReason  string
	FailureMessage string
}

// msgSessionStarting is emitted once, when the user submits their first
// message: the AgentSession is being created and the session-scoped
// pipeline is coming up. It flips the TUI into a visible "starting"
// state until the first agent output or a phase update arrives.
type msgSessionStarting struct{}

// tickMsg is the bubbletea message that drives the session elapsed timer.
// It is emitted ~once per second while the session is live and not ended.
type tickMsg time.Time

// msgTimelineFlush repaints a timeline that a streamed run left dirty. See
// markTimelineDirty.
type msgTimelineFlush struct{}

// timelineFlushInterval bounds how stale the transcript can be during a
// streamed run. Short enough that the text still reads as live typing, long
// enough that a fast stream cannot drive a re-render per token.
const timelineFlushInterval = 40 * time.Millisecond

// msgConnState carries a connectivity-state change (NATS / memory-API
// port-forward up/reconnecting/down).
type msgConnState struct{ State connState }

// msgInboundResult carries the InboundDecision from an async
// SubmitUserMessage so a permission-deny, a delivery failure, or a
// "session has ended" outcome surfaces in the timeline. Routed=false
// is ALWAYS rendered (no silent errors).
//
// Ended is set when the pipeline reported OutcomeNoActiveSession — the
// session went terminal and the pipeline (correctly) refused to spawn a
// phantom session for this `local` channel. The TUI treats it exactly
// like a terminal phase: end the interaction, disable input.
type msgInboundResult struct {
	Routed bool
	// Notice is the structured user-facing message for this inbound, or nil.
	// It carries its own severity, which is what lets the timeline colour a
	// dead session differently from a retryable hiccup.
	Notice *channelevents.NoticeWire
	Err    string
	Ended  bool
}

type connState int

const (
	connConnected connState = iota
	connReconnecting
	connDown
)

// --- timeline item model ---

// itemKind discriminates a timeline entry.
type itemKind int

const (
	itemUser        itemKind = iota // a "you" message
	itemAgent                       // an "agent" message
	itemToolSession                 // a collapsible tool-session block
	itemNote                        // a system note (permission request, send error, session ended)
)

// timelineItem is one entry in the scrolling transcript.
type timelineItem struct {
	kind itemKind
	text string // user/agent/note body
	at   time.Time
	tool *toolSessionState // non-nil only for itemToolSession
}

// toolSessionState is the mutable state of one inline tool-session block.
type toolSessionState struct {
	ref       string   // ToolCallRef — correlation key
	header    string   // outer tool + reason ("claude · write the README")
	tail      []string // recent output lines (line-bounded)
	collapsed bool
	done      bool
	ok        bool
	costUSD   float64
	elapsed   time.Duration
	startedAt time.Time
}

const toolTailMax = 6 // visible tail lines for a running tool (line-bounded)

// --- the model ---

// chatModel is the bubbletea model for `oap agent chat`.
type chatModel struct {
	agentClass  string
	sessionName string

	items   []timelineItem
	toolIdx map[string]int // ToolCallRef → index into items

	plan      *channelevents.PlanUpdatePayload // latest plan snapshot; nil = no plan
	planOpen  bool                             // plan overlay visible
	statusTxt string                           // latest update_status text
	conn      connState

	// interaction modal: non-nil while a generic-interaction decision prompt
	// is blocking. Category-generic — the actionable keys are derived from the
	// payload's ActionKindDecision actions (see decisionBindings), never from a
	// per-category branch. tool_approval, info leakage, identity choice, … all
	// render through this one modal.
	interaction *channelevents.InteractionRequestPayload
	// interactionFrom is the session the pending prompt is ABOUT, taken from
	// the render event rather than from interaction.AgentSessionRef: the former
	// is the relay's subject-checked identity, the latter is publisher JSON.
	// They normally agree; when they do not, only one of them is a fact.
	interactionFrom local.SessionRef

	// stream coalescing: text_delta runs append to a pending agent item.
	streaming bool
	// timelineDirty marks items as changed with the viewport not yet
	// re-rendered — a repaint is already scheduled (see markTimelineDirty).
	timelineDirty bool
	// timelineRenders counts full-transcript re-renders. It exists so the
	// coalescing invariant is assertable: "a streamed run repaints a bounded
	// number of times, not once per token" is otherwise invisible until a
	// long session goes sluggish, which no test would catch.
	timelineRenders int

	input       textinput.Model
	vp          viewport.Model
	width       int
	height      int
	ready       bool
	starting    bool // first message submitted; session is coming up
	sessionLive bool // AgentSession creation has begun; the id is now real
	ended       bool // session reached a terminal phase; input disabled
	endMsg      string

	// session elapsed timer: sessionStartedAt is set on msgSessionStarting;
	// sessionEndedAt is set when the session reaches a terminal state.
	// Both are zero until the respective transition occurs.
	sessionStartedAt time.Time
	sessionEndedAt   time.Time

	// th is resolved once, at construction. A long-lived model re-renders on
	// every keystroke, stream delta and timer tick, so re-detecting
	// capabilities per frame would put a terminal probe and a full style build
	// in the hot path.
	th *tui.Theme
	// submit is invoked on Enter with the typed text; set by the command
	// wiring (Task 7's host Listener). nil in unit tests.
	submit func(text string)
	// decideInteraction is invoked on a generic-interaction decision keypress
	// (category-generic tool_approval / identity_choice / …); set by the
	// wiring. nil in unit tests unless the test sets it.
	decideInteraction func(requestRef, category, actionID string)
}

// newChatModel builds the model for a real run. Capabilities are read off
// os.Stdout because that is the stream bubbletea draws on whatever writer the
// command was given, so --no-color, NO_COLOR and a non-terminal stdout each
// land on the colorless theme here exactly as they do elsewhere.
func newChatModel(agentClass, sessionName string, noColor bool) chatModel {
	return newChatModelWithTheme(agentClass, sessionName, tui.NewTheme(tui.Detect(os.Stdout, noColor)))
}

// newChatModelWithTheme builds the model against a caller-supplied theme.
// Layout is zero until the first WindowSizeMsg.
//
// It exists so the rendering can be exercised at capabilities this process does
// not have: a test binary's stdout is never a terminal, so newChatModel can only
// ever produce a colorless theme, and a render path that quietly stopped
// consulting the theme would look identical.
func newChatModelWithTheme(agentClass, sessionName string, th *tui.Theme) chatModel {
	ti := textinput.New()
	ti.Placeholder = "type a message…"
	ti.Prompt = "› "
	// bubbles dresses its own prompt and placeholder in a hardcoded gray that
	// is not in oap's palette and does not follow Caps.Color. Point both at the
	// theme so the input box is subtle for the same reason every other hint in
	// `oap` is, and goes plain when color is off.
	ti.PromptStyle = th.Subtle
	ti.PlaceholderStyle = th.Subtle
	ti.Focus()
	return chatModel{
		agentClass:  agentClass,
		sessionName: sessionName,
		toolIdx:     map[string]int{},
		conn:        connConnected,
		input:       ti,
		th:          th,
	}
}

func (m chatModel) Init() tea.Cmd { return textinput.Blink }

// layout recomputes pane dimensions from the terminal size. Chrome:
// title line + status line + input line + footer line, plus the
// timeline pane's rounded border (top+bottom).
func (m *chatModel) layout() {
	innerW := m.width - 2 // timeline box border left+right
	if innerW < 20 {
		innerW = 20
	}
	// title(1) + status(1) + input(1) + footer(1) + box border(2) = 6
	innerH := m.height - 6
	if innerH < 3 {
		innerH = 3
	}
	m.vp.Width = innerW
	m.vp.Height = innerH
	m.input.Width = innerW - 3
}

// refreshTimeline re-renders every timeline item into the viewport and
// pins the scroll to the bottom (newest visible) unless the user has
// scrolled up.
//
// This is the expensive operation in the model: it word-wraps and styles the
// WHOLE transcript, so its cost grows with the conversation. Callers on a
// per-token path must go through markTimelineDirty instead.
// markTimelineDirty records that the timeline needs repainting and returns the
// command that will do it — or nil when a repaint is already pending, so a
// fast stream schedules one timer per run rather than one per token.
func (m *chatModel) markTimelineDirty() tea.Cmd {
	if m.timelineDirty {
		return nil
	}
	m.timelineDirty = true
	return tea.Tick(timelineFlushInterval, func(time.Time) tea.Msg { return msgTimelineFlush{} })
}

func (m *chatModel) refreshTimeline() {
	m.timelineDirty = false
	m.timelineRenders++
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(renderTimeline(m.items, m.vp.Width, m.th))
	if atBottom {
		m.vp.GotoBottom()
	}
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.vp.Width == 0 && m.vp.Height == 0 {
			m.vp = viewport.New(1, 1)
		}
		m.layout()
		m.ready = true
		m.refreshTimeline()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	// --- local-kind render events ---
	case local.MsgUserMessage:
		m.starting = false
		m.appendAgentMessage(msg.Text)
		m.streaming = false
		m.refreshTimeline()
		return m, nil
	case msgLocalEcho:
		m.items = append(m.items, timelineItem{kind: itemUser, text: msg.Text, at: time.Now()})
		m.refreshTimeline()
		return m, nil
	case local.MsgNotification:
		m.statusTxt = msg.Text
		return m, nil
	case local.MsgOperationActivity:
		// A bare Cleared tick carries no line — don't blank the status on it;
		// the channelsd relay's EffectiveLine resolution already reverts to the
		// remembered caption before a real clear is sent.
		if msg.Payload.CompactLine != "" {
			m.statusTxt = msg.Payload.CompactLine
		}
		return m, nil
	case local.MsgPlanUpdate:
		p := msg.Payload
		m.plan = &p
		return m, nil
	case local.MsgToolSessionEvent:
		m.starting = false
		m.applyToolEvent(msg.Payload)
		m.refreshTimeline()
		return m, nil
	case local.MsgToolSessionDelta:
		m.starting = false
		m.applyToolDelta(msg.Payload)
		m.refreshTimeline()
		return m, nil
	case local.MsgStreamDelta:
		m.starting = false
		m.applyStreamDelta(msg.Payload)
		// text_delta is the only per-token message the model receives — one
		// per llm.StreamEventTextDelta, forwarded with no debounce — so it is
		// the one path that must not drive a full-transcript repaint. Every
		// other event type (notably "stop") is rare and paints synchronously,
		// which is also what guarantees a run cannot end with its tail
		// unpainted behind a timer nothing re-arms.
		if msg.Payload.EventType == "text_delta" {
			return m, m.markTimelineDirty()
		}
		m.refreshTimeline()
		return m, nil
	case msgTimelineFlush:
		if m.timelineDirty {
			m.refreshTimeline()
		}
		return m, nil
	case local.MsgInteractionRequest:
		// A generic interaction carrying decision actions opens a blocking
		// decision modal; one with no decision actions (link / read-only
		// notice) has nothing to decide, so it renders its pre-rendered
		// markdown floor to the timeline instead.
		if len(decisionBindings(msg.Payload)) > 0 {
			p := msg.Payload
			m.interaction = &p
			m.interactionFrom = msg.Session
			return m, nil
		}
		if msg.Text != "" {
			m.items = append(m.items, timelineItem{kind: itemNote, at: time.Now(), text: msg.Text})
			m.refreshTimeline()
		}
		return m, nil
	case local.MsgInteractionApplied:
		// A decision (or timeout) resolved the interaction: dismiss any open
		// decision modal (so a remote resolution or timeout can't strand the
		// modal) and record the outcome.
		// Unconditional: this TUI is the only surface a single-user session
		// has, so an outcome it drops is an outcome nobody ever sees. Gating
		// on OutcomeText left every tool approval silent — both tool-approval
		// decision handlers resolve with no OutcomeText — and rendered
		// identity_choice's raw wire action id when it was set.
		m.interaction = nil
		m.interactionFrom = local.SessionRef{}
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: categories.OutcomeLabel(msg.Category, msg.OutcomeText, msg.Outcome),
		})
		m.refreshTimeline()
		return m, nil
	case local.MsgInteractionRejected:
		// A per-clicker decision rejection: THIS TUI's click was refused (lacked
		// standing, the prompt was already resolved by someone else, a category
		// mismatch, or the bound handler errored — e.g. a tool_approval
		// grant-write failure). handleKey already dismissed the decision modal
		// optimistically at click time, so there's nothing to tear down here —
		// just append a visible timeline note so a real failure (handler_error)
		// is never silently dropped (no-silent-errors, AGENTS.md).
		// already_resolved is informational (dimmed, mirroring the "starting the
		// agent…" note); every other class is a genuine failure, styled like
		// MsgSendError's warning.
		text := interactionRejectionText(msg)
		if msg.Class == "already_resolved" {
			text = m.th.Render(m.th.Subtle, text)
		} else {
			text = m.th.Render(m.th.Warn, text)
		}
		m.items = append(m.items, timelineItem{kind: itemNote, at: time.Now(), text: text})
		m.refreshTimeline()
		return m, nil
	case local.MsgPermissionRequest:
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: "session-join request: " + msg.Payload.Preview,
		})
		m.refreshTimeline()
		return m, nil
	case local.MsgPermissionDecisionApplied:
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: "session-join " + msg.Payload.Decision,
		})
		m.refreshTimeline()
		return m, nil
	case local.MsgLiveViewOffer:
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: "View in browser: " + msg.URL,
		})
		m.refreshTimeline()
		return m, nil
	case local.MsgAgentUIOffer:
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: "Open the dashboard: " + msg.URL,
		})
		m.refreshTimeline()
		return m, nil
	case msgLiveViewUnavailable:
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: m.th.Render(m.th.Subtle, "browser view links unavailable: "+msg.Reason),
		})
		m.refreshTimeline()
		return m, nil
	case msgStartupNotice:
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: m.th.Render(m.th.Subtle, msg.Text),
		})
		m.refreshTimeline()
		return m, nil
	case local.MsgSendError:
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: msg.At,
			text: m.th.Render(m.th.Warn, "render error ("+string(msg.Kind)+"): "+msg.Err),
		})
		m.refreshTimeline()
		return m, nil

	// --- TUI-internal events ---
	case msgSessionStarting:
		m.starting = true
		// The AgentSession is being created now, so its id is finally
		// real — let the header display it from here on.
		m.sessionLive = true
		// Record when the session started so the elapsed timer can
		// compute time.Since(sessionStartedAt) at render time.
		m.sessionStartedAt = time.Now()
		// This timeline note is a permanent record of the startup phase:
		// it intentionally stays in the timeline after m.starting is
		// cleared (by the first agent output / phase update). Only the
		// transient status-line "starting the agent…" indicator follows
		// m.starting — the note is history, not a missing cleanup.
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: m.th.Render(m.th.Subtle, "starting the agent…"),
		})
		m.refreshTimeline()
		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
	case tickMsg:
		// Re-issue the tick while the session is live and not yet ended so
		// the header elapsed timer refreshes ~every second. Once ended the
		// displayed elapsed is frozen at sessionEndedAt.Sub(sessionStartedAt)
		// and no further ticks are needed.
		if m.sessionLive && !m.ended {
			return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
		}
		return m, nil
	case msgSessionPhase:
		return m.handlePhase(msg), nil
	case msgConnState:
		m.conn = msg.State
		return m, nil
	case msgInboundResult:
		// No silent errors: a non-routed inbound is ALWAYS surfaced
		// visibly in the timeline — never dropped. Routed results need
		// no note (the agent's reply is the feedback).
		if !msg.Routed {
			switch {
			case msg.Ended:
				// The session has gone terminal — the pipeline refused
				// to spawn a phantom session. End the interaction in the
				// TUI exactly as a terminal phase would, so the user sees
				// it and can only quit.
				if !m.ended {
					m.ended = true
					m.sessionEndedAt = time.Now()
					m.endMsg = "session ended"
					m.items = append(m.items, timelineItem{
						kind: itemNote, at: time.Now(),
						text: m.th.Render(m.th.Err, "✗ the session has ended — your message was not delivered"),
					})
					m.items = append(m.items, timelineItem{
						kind: itemNote, at: time.Now(),
						text: m.th.Render(m.th.Subtle, "the interaction has ended — press q to quit"),
					})
				}
			case !msg.Notice.IsZero():
				// Style by the notice's own severity, so a terminal refusal
				// and a retryable hiccup are distinguishable at a glance
				// rather than sharing one warning colour.
				text := msg.Notice.Text()
				var styled string
				switch notice.WireTone(msg.Notice) {
				case channelinteractions.ToneCritical:
					styled = m.th.Render(m.th.Err, text)
				case channelinteractions.ToneResolved:
					styled = m.th.Render(m.th.Success, text)
				default:
					styled = m.th.Render(m.th.Warn, text)
				}
				m.items = append(m.items, timelineItem{
					kind: itemNote, at: time.Now(), text: styled,
				})
			case msg.Err != "":
				m.items = append(m.items, timelineItem{
					kind: itemNote, at: time.Now(),
					text: m.th.Render(m.th.Err, "✗ message not delivered: "+msg.Err),
				})
			default:
				// Defensive: a non-routed result with no detail is still
				// a delivery failure — surface it rather than swallow it.
				m.items = append(m.items, timelineItem{
					kind: itemNote, at: time.Now(),
					text: m.th.Render(m.th.Err, "✗ message not delivered (no detail reported)"),
				})
			}
			m.refreshTimeline()
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// appendAgentMessage appends (or, when streaming, finalizes) an agent
// message. A streaming run that just ended is replaced wholesale by the
// authoritative respond_to_user text.
func (m *chatModel) appendAgentMessage(text string) {
	if m.streaming && len(m.items) > 0 && m.items[len(m.items)-1].kind == itemAgent {
		m.items[len(m.items)-1].text = text
		return
	}
	m.items = append(m.items, timelineItem{kind: itemAgent, text: text, at: time.Now()})
}

// applyStreamDelta coalesces a live LLM stream event into the timeline.
// text_delta runs append to a trailing agent item; tool_use_start opens
// a tool-session block keyed by ToolID.
func (m *chatModel) applyStreamDelta(p channelevents.AssistantStreamDeltaPayload) {
	switch p.EventType {
	case "text_delta":
		if !m.streaming || len(m.items) == 0 || m.items[len(m.items)-1].kind != itemAgent {
			m.items = append(m.items, timelineItem{kind: itemAgent, at: time.Now()})
			m.streaming = true
		}
		m.items[len(m.items)-1].text += p.Text
	case "stop":
		m.streaming = false
	}
}

// applyToolEvent updates (or creates) the inline tool-session block for
// a parsed interactive-tool event.
func (m *chatModel) applyToolEvent(p channelevents.ToolSessionEventPayload) {
	ts := m.toolFor(p.ToolCallRef, p.OuterTool, p.Reason)
	switch p.EventType {
	case "tool_use_start":
		// Mirror the slack renderer's eventTranscriptLine: show the
		// sub-tool name AND its input summary — the command/args the
		// streaming agent (e.g. Claude Code) is actually running — not
		// just the bare tool name.
		line := "→ " + p.ToolName
		if p.Summary != "" {
			if p.ToolName != "" {
				line = "→ " + p.ToolName + " " + p.Summary
			} else {
				line = "→ " + p.Summary
			}
		}
		ts.tail = appendTail(ts.tail, line)
	case "tool_use_stop":
		mark := "✓"
		if !p.OK {
			mark = "✗"
		}
		line := mark
		if p.Summary != "" {
			line = mark + " " + p.Summary
		}
		ts.tail = appendTail(ts.tail, line)
	case "text_delta":
		for _, ln := range strings.Split(strings.TrimRight(p.Text, "\n"), "\n") {
			ts.tail = appendTail(ts.tail, ln)
		}
	case "result":
		ts.done = true
		ts.ok = p.OK
		ts.costUSD = p.CostUSD
		ts.elapsed = time.Duration(p.DurationMs) * time.Millisecond
	}
}

// applyToolDelta updates the inline tool-session block for a raw output
// chunk. The terminal delta marks the block done.
func (m *chatModel) applyToolDelta(p channelevents.ToolSessionDeltaPayload) {
	ts := m.toolFor(p.ToolCallRef, "", "")
	if len(p.Data) > 0 {
		for _, ln := range strings.Split(strings.TrimRight(string(p.Data), "\n"), "\n") {
			if ln != "" {
				ts.tail = appendTail(ts.tail, ln)
			}
		}
	}
	if p.Terminal {
		ts.done = true
		ts.ok = p.ExitReason == "completed" || p.ExitCode == 0
		if !ts.startedAt.IsZero() {
			ts.elapsed = time.Since(ts.startedAt)
		}
	}
}

// toolFor returns the toolSessionState for ref, creating the timeline
// item on first sight. header is filled from the first event that
// carries an outer tool / reason.
func (m *chatModel) toolFor(ref, outerTool, reason string) *toolSessionState {
	if idx, ok := m.toolIdx[ref]; ok {
		ts := m.items[idx].tool
		if ts.header == "" && outerTool != "" {
			ts.header = toolHeader(outerTool, reason)
		}
		return ts
	}
	ts := &toolSessionState{ref: ref, header: toolHeader(outerTool, reason), startedAt: time.Now()}
	m.items = append(m.items, timelineItem{kind: itemToolSession, tool: ts, at: time.Now()})
	m.toolIdx[ref] = len(m.items) - 1
	return ts
}

func toolHeader(outerTool, reason string) string {
	if outerTool == "" {
		return ""
	}
	if reason == "" {
		return outerTool
	}
	return outerTool + " · " + reason
}

// appendTail appends a line to a line-bounded tail buffer, dropping the
// oldest line past toolTailMax — the same line-bounded tail the Slack
// renderer uses.
func appendTail(tail []string, line string) []string {
	tail = append(tail, line)
	if len(tail) > toolTailMax {
		tail = tail[len(tail)-toolTailMax:]
	}
	return tail
}

// interactionRejectionText renders the one-line note shown for a
// local.MsgInteractionRejected: the pipe never swallows a rejected click
// (no-silent-errors), so this TUI must show SOMETHING even though the
// generic-interaction modal itself is already gone (dismissed optimistically
// at click time). Mirrors Slack's interactionRejectionText
// (pkg/channels/channelkinds/slack/interaction.go) and the webchat React case
// (pkg/web/webui/chat/ui/ChatApp.tsx's interactionRejectionText) minus the
// Slack-only <@mention> — the local Msg carries no OriginalDecider, only
// OriginalOutcome, so already_resolved names the outcome, not who applied it.
func interactionRejectionText(msg local.MsgInteractionRejected) string {
	reason := strings.TrimSpace(msg.Reason)
	if reason == "" {
		reason = "your click could not be applied"
	}
	if msg.Class == "already_resolved" && msg.OriginalOutcome != "" {
		reason += " (" + msg.OriginalOutcome + ")"
	}
	return reason + "."
}

// handlePhase reacts to a session phase transition. A terminal phase
// (Failed or Succeeded) ENDS the interaction: it disables the input box
// (m.ended) and renders the outcome PROMINENTLY in the timeline so the
// user is never left guessing. A Failed phase renders the AgentSession's
// failure reason AND (when present) the condition message — the "what",
// not just the bare phase word. handlePhase is idempotent: a second
// terminal phase (the watcher only sends one, but defensively) does not
// append a duplicate note.
func (m chatModel) handlePhase(msg msgSessionPhase) chatModel {
	m.starting = false
	if m.ended {
		// Already terminal — the watcher sends a single terminal phase,
		// but guard so a stray repeat can't append a second banner.
		return m
	}
	switch msg.Phase {
	case "Failed":
		m.ended = true
		m.sessionEndedAt = time.Now()
		m.endMsg = "session failed — " + sessionFailureSummary(msg)
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: m.th.Render(m.th.Err, "✗ "+m.endMsg),
		})
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: m.th.Render(m.th.Subtle, "the interaction has ended — press q to quit"),
		})
	case "Succeeded":
		m.ended = true
		m.sessionEndedAt = time.Now()
		m.endMsg = "session ended"
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: m.th.Render(m.th.Success, "✓ "+m.endMsg),
		})
		m.items = append(m.items, timelineItem{
			kind: itemNote, at: time.Now(),
			text: m.th.Render(m.th.Subtle, "the interaction has ended — press q to quit"),
		})
	default:
		// A non-terminal phase update — nothing to render.
		return m
	}
	m.refreshTimeline()
	return m
}

// sessionFailureSummary joins the failure reason and message into one
// operator-facing line: "Reason: message" when both are present, just
// the reason (or message, or "unknown") otherwise.
func sessionFailureSummary(msg msgSessionPhase) string {
	switch {
	case msg.FailureReason != "" && msg.FailureMessage != "":
		return msg.FailureReason + ": " + msg.FailureMessage
	case msg.FailureReason != "":
		return msg.FailureReason
	case msg.FailureMessage != "":
		return msg.FailureMessage
	default:
		return "unknown"
	}
}

// handleKey routes key presses by focus: the generic interaction decision
// modal blocks all input until decided; the plan overlay consumes p/esc;
// otherwise the input box, timeline scroll, and collapse toggle share keys.
func (m chatModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// ctrl+c always quits.
	if key == "ctrl+c" {
		return m, tea.Quit
	}

	// Generic-interaction decision modal has exclusive focus. Category-generic:
	// the actionable keys come from the payload's ActionKindDecision actions
	// (decisionBindings), never an `if category == "tool_approval"` branch. The
	// matched key publishes the decision via m.decideInteraction and dismisses
	// the modal (optimistic dismiss).
	if m.interaction != nil {
		for _, bnd := range decisionBindings(*m.interaction) {
			if key == bnd.Key {
				if m.decideInteraction != nil {
					m.decideInteraction(m.interaction.RequestRef, m.interaction.Category, bnd.ActionID)
				}
				m.interaction = nil
				return m, nil
			}
		}
		return m, nil // swallow every other key while blocked
	}

	// Plan overlay.
	if m.planOpen {
		if key == "p" || key == "esc" || key == "q" {
			m.planOpen = false
		}
		return m, nil
	}

	// Terminal session: the interaction is over. The input box is
	// genuinely dead from here — no key may reach m.input or m.submit
	// (a message arriving at the pipeline after the session ended is the
	// exact bug we're fixing). The user can still quit and scroll the
	// transcript; q/esc need NO empty-input guard because nothing can be
	// typed after this point. Every other key (typed runes, enter,
	// space, p) is swallowed.
	if m.ended {
		switch key {
		case "q", "esc":
			return m, tea.Quit
		case "up", "down", "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	switch key {
	case "q":
		// q quits only when the input box is empty — otherwise it is text.
		if strings.TrimSpace(m.input.Value()) == "" {
			return m, tea.Quit
		}
	case "p":
		if strings.TrimSpace(m.input.Value()) == "" {
			m.planOpen = m.plan != nil
			return m, nil
		}
	case "esc":
		return m, tea.Quit
	case "up", "down", "pgup", "pgdown":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case " ":
		// space toggles the collapse state of the newest tool session
		// when the input box is empty.
		if strings.TrimSpace(m.input.Value()) == "" {
			m.toggleNewestTool()
			m.refreshTimeline()
			return m, nil
		}
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		// m.ended is already handled by the terminal-session block above;
		// the check here is defensive — submit must NEVER run once the
		// session is terminal.
		if text == "" || m.ended {
			return m, nil
		}
		m.input.Reset()
		m.items = append(m.items, timelineItem{kind: itemUser, text: text, at: time.Now()})
		m.refreshTimeline()
		if m.submit != nil {
			m.submit(text)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// toggleNewestTool flips the collapsed flag on the most recent
// tool-session item, if any.
func (m *chatModel) toggleNewestTool() {
	for i := len(m.items) - 1; i >= 0; i-- {
		if m.items[i].kind == itemToolSession {
			m.items[i].tool.collapsed = !m.items[i].tool.collapsed
			return
		}
	}
}

func (m chatModel) View() string {
	if !m.ready {
		return "loading…"
	}
	// The session id is only shown once the AgentSession actually
	// exists — creation begins on the first user message (the
	// msgSessionStarting transition sets sessionLive). Before that,
	// showing a live-looking id would be misleading, so the header
	// carries just the agent-class plus a hint to start typing.
	titleLeft := m.th.Render(m.th.Title, fmt.Sprintf("oap agent chat — %s", m.agentClass))
	if m.sessionLive {
		titleLeft += m.th.Render(m.th.Subtle, "   "+m.sessionName)
	} else {
		titleLeft += m.th.Render(m.th.Subtle, "   (type a message to start a session)")
	}
	// Session elapsed timer: shown only once the session is live.
	// Elapsed is computed at render time (not by counting ticks) to avoid
	// drift. Once ended, the duration is frozen at sessionEndedAt−sessionStartedAt.
	title := titleLeft
	if m.sessionLive && !m.sessionStartedAt.IsZero() {
		var elapsed time.Duration
		if m.ended && !m.sessionEndedAt.IsZero() {
			elapsed = m.sessionEndedAt.Sub(m.sessionStartedAt)
		} else {
			elapsed = time.Since(m.sessionStartedAt)
		}
		timerStr := m.th.Render(m.th.Subtle, fmtSessionElapsed(elapsed))
		leftW := lipgloss.Width(titleLeft)
		timerW := lipgloss.Width(timerStr)
		gap := m.width - leftW - timerW
		if gap >= 1 {
			title = titleLeft + strings.Repeat(" ", gap) + timerStr
		}
	}
	// Chrome's rule, applied at the width this model actually has: a header
	// that overflowed would wrap, taking a row that layout() already handed to
	// the timeline and leaving the bottom line of the frame off-screen. A long
	// agent-class name on a narrow terminal is enough to trigger it.
	title = tui.Truncate(title, m.width)
	timeline := m.th.Frame.Width(m.vp.Width).Height(m.vp.Height).Render(m.vp.View())
	status := renderStatusLine(m.statusTxt, m.conn, m.plan, m.starting, m.width, m.th)
	// Once the session is terminal the input box is replaced wholesale by
	// a persistent, prominent "interaction over" indicator — there is no
	// live textinput to render, the user cannot send, and the footer
	// switches to a quit-only hint. Before that, the normal input box +
	// full keymap footer.
	inputLine := m.input.View()
	footer := m.th.Render(m.th.Subtle, "enter send · ↑↓ scroll · space collapse · p plan · q quit")
	if m.ended {
		inputLine = m.th.Render(m.th.Err, "✗ "+m.endMsg+" — input disabled")
		footer = m.th.Render(m.th.Warn, "— session ended · press q to quit —")
	}

	base := lipgloss.JoinVertical(lipgloss.Left, title, timeline, status, inputLine, footer)

	// Overlays draw on top by replacing the whole frame — bubbletea has
	// no compositing; an overlay is a full-screen alternate View.
	if m.interaction != nil {
		return renderInteractionModal(*m.interaction, m.interactionFrom, m.sessionName, m.width, m.height, m.th)
	}
	if m.planOpen && m.plan != nil {
		return renderPlanOverlay(*m.plan, m.width, m.height, m.th)
	}
	return base
}

// renderTimeline renders every timeline item, width-wrapped, into one
// string for the viewport.
func renderTimeline(items []timelineItem, width int, th *tui.Theme) string {
	if len(items) == 0 {
		return th.Render(th.Subtle, "(no messages yet — type below to start)")
	}
	wrap := lipgloss.NewStyle().Width(width)
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		switch it.kind {
		case itemUser:
			b.WriteString(wrap.Render(th.Render(th.RoleUser, "you") + " · " + it.text))
		case itemAgent:
			b.WriteString(wrap.Render(th.Render(th.RoleAssistant, "agent") + " · " + it.text))
		case itemNote:
			b.WriteString(wrap.Render(it.text))
		case itemToolSession:
			b.WriteString(renderToolBlock(it.tool, width, th))
		}
	}
	return b.String()
}

// renderToolBlock renders one inline tool-session block: a header line
// with the ▶/▼ collapse glyph + done/cost/elapsed, then (when expanded)
// the line-bounded tail indented under a "│" gutter.
func renderToolBlock(ts *toolSessionState, width int, th *tui.Theme) string {
	glyph := "▼"
	if ts.collapsed {
		glyph = "▶"
	}
	header := ts.header
	if header == "" {
		header = "tool"
	}
	var trailer string
	switch {
	case ts.done && ts.ok:
		trailer = th.Render(th.Success, fmt.Sprintf("  ✓ done · $%.4f · %s", ts.costUSD, fmtElapsed(ts.elapsed)))
	case ts.done && !ts.ok:
		trailer = th.Render(th.Err, "  ✗ failed · "+fmtElapsed(ts.elapsed))
	default:
		trailer = th.Render(th.Subtle, "  …running")
	}
	line := th.Render(th.Subtle, glyph) + " " + th.Render(th.RoleTool, header) + trailer
	if ts.collapsed {
		return line
	}
	var b strings.Builder
	b.WriteString(line)
	for _, ln := range ts.tail {
		b.WriteString("\n  " + th.Render(th.Subtle, "│ ") + ln)
	}
	return b.String()
}

func fmtElapsed(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// fmtSessionElapsed formats a session elapsed duration as a compact string
// for the header timer: "0s", "45s", "1m23s", "1h02m". Negative or zero
// durations render as "0s".
func fmtSessionElapsed(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// renderStatusLine renders the status pane: a connection-state dot, the
// latest update_status text, and a compact plan indicator. While the
// session is starting (first message submitted, runner not yet
// producing output) it shows a distinct "starting the agent…" state
// rather than a bare "idle".
func renderStatusLine(statusTxt string, conn connState, plan *channelevents.PlanUpdatePayload, starting bool, width int, th *tui.Theme) string {
	var dot string
	switch conn {
	case connConnected:
		dot = th.Render(th.Success, "●")
	case connReconnecting:
		dot = th.Render(th.Warn, "◐")
	case connDown:
		dot = th.Render(th.Err, "○")
	}
	left := dot + " "
	switch {
	case statusTxt != "":
		left += statusTxt
	case starting:
		left += th.Render(th.Warn, "starting the agent…")
	default:
		left += th.Render(th.Subtle, "idle")
	}
	right := ""
	if plan != nil {
		done := 0
		for _, it := range plan.Items {
			if it.Status == "done" {
				done++
			}
		}
		right = th.Render(th.Subtle, fmt.Sprintf("📋 plan %d/%d", done, len(plan.Items)))
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// renderPlanOverlay renders the full plan as a checklist overlay.
func renderPlanOverlay(plan channelevents.PlanUpdatePayload, width, height int, th *tui.Theme) string {
	var b strings.Builder
	b.WriteString(th.Render(th.Title, "Plan — "+plan.PlanName))
	b.WriteString("\n\n")
	for _, it := range plan.Items {
		var mark string
		switch it.Status {
		case "done":
			mark = th.Render(th.Success, "[x]")
		case "in_progress":
			mark = th.Render(th.Warn, "[~]")
		case "error":
			mark = th.Render(th.Err, "[!]")
		default:
			mark = th.Render(th.Subtle, "[ ]")
		}
		b.WriteString(mark + " " + it.Label + "\n")
	}
	b.WriteString("\n" + th.Render(th.Subtle, "p/esc close"))
	inner := th.Frame.Width(width - 4).Render(b.String())
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, inner)
}

// decisionBinding is one keyboard-bound decision action in the generic
// interaction modal: press Key to publish a decision with ActionID.
type decisionBinding struct {
	Key      string
	ActionID string
	Label    string
}

// decisionBindings returns the ordered key→action bindings for a generic
// interaction's decision actions. Each ActionKindDecision action binds the
// first rune of its ID (lowercased) — so tool_approval's "approve"/"deny" map
// to a/d, identity_choice's "agent"/"userPassthrough"/"cancel" map to a/u/c,
// and so on. This is deliberately category-generic: the keys come from the
// actions, never an `if category == …` branch. Link / link_mint actions carry
// no binding (read-only). On a first-rune collision the earlier action (by
// slice order) wins, so the result is deterministic.
func decisionBindings(p channelevents.InteractionRequestPayload) []decisionBinding {
	var out []decisionBinding
	taken := map[string]bool{}
	for _, a := range p.Actions {
		if a.Kind != channelevents.ActionKindDecision || a.ID == "" {
			continue
		}
		key := strings.ToLower(string([]rune(a.ID)[0]))
		if taken[key] {
			continue
		}
		taken[key] = true
		out = append(out, decisionBinding{Key: key, ActionID: a.ID, Label: a.Label})
	}
	return out
}

// renderInteractionModal renders the blocking generic-interaction decision
// modal. It is category-generic: the actionable keys are derived from the
// payload's decision actions (decisionBindings), never from the category.
// Link / link_mint actions render as read-only labels.
// askingSessionLine names the session a prompt came from, but ONLY when it is
// not the one this TUI is showing.
//
// A card about a DELEGATED CHILD is delivered here — the relay resolves a
// human-directed envelope up the lineage to the nearest binding a person reads,
// which for a delegated child is this host's own root. So the person is
// deciding on behalf of a session they are not watching and never started,
// and without this the modal reads as though their own agent is asking.
//
// Empty for the ordinary case. Restating "session default/the-one-you-are-in"
// on every prompt is noise, and noise is what makes the one line that matters
// invisible.
//
// THE ASKER IS PASSED IN, not read off the payload. InteractionRequestPayload
// carries an AgentSessionRef, but it is publisher-controlled JSON that its own
// doc calls "not validated here" — while the ref the local kind emits comes
// from the SessionInfo the relay built after cross-checking the envelope
// against its NATS subject. Taking the payload's copy would make a suppressible
// claim decide what the approver is told: a runner writing the WATCHED
// session's name into it makes this line vanish, precisely when it matters.
// The relay now also corrects that field (stampAskingSession), so this is the
// second of two locks rather than the only one.
func askingSessionLine(asker local.SessionRef, ownSession string) string {
	if asker.Name == "" || asker.Name == ownSession {
		return ""
	}
	if asker.Namespace != "" {
		return "asked by session " + asker.Namespace + "/" + asker.Name
	}
	return "asked by session " + asker.Name
}

// askingChainLine says how the asking session RELATES to the one on screen.
//
// The spec calls this material to the decision — "this is a grandchild of the
// session you are in" — and it is the difference between approving something
// your agent delegated and approving something two removes away that you never
// named. The chain is platform-stamped by the relay from cluster lineage
// (AskingChain), never by the asking agent, for the same reason the asker
// itself is corrected there.
//
// Empty when there is nothing to explain: no chain, or a chain of one, which is
// a card delivered through the asking session's own binding.
func askingChainLine(chain []channelevents.SessionRef, ownSession string) string {
	hops := len(chain) - 1
	if hops < 1 {
		return ""
	}
	// The far end is the session whose channel this arrived on — normally the
	// one on screen. Name it only when it is not, so the sentence stays about
	// the relationship rather than restating where the reader already is.
	anchor := chain[len(chain)-1].Name
	if anchor == "" || anchor == ownSession {
		anchor = "this session"
	}
	// Each arm composes the WHOLE phrase. Building "<rel> of <anchor>" from a
	// shared suffix reads fine for the noun forms and produces "3 levels below
	// of this session" for the counted one.
	switch hops {
	case 1:
		return "a subagent of " + anchor
	case 2:
		return "a grandchild of " + anchor
	default:
		return fmt.Sprintf("%d levels below %s", hops, anchor)
	}
}

func renderInteractionModal(req channelevents.InteractionRequestPayload, asker local.SessionRef, ownSession string, width, height int, th *tui.Theme) string {
	var b strings.Builder
	b.WriteString(th.Render(th.Title, "Decision required"))
	b.WriteString("\n\n")
	// Above the lead, not in a footer: this changes what the question MEANS,
	// so it has to be read before the question rather than after the decision.
	if line := askingSessionLine(asker, ownSession); line != "" {
		if rel := askingChainLine(req.AskingChain, ownSession); rel != "" {
			line += " (" + rel + ")"
		}
		b.WriteString(th.Render(th.Subtle, line) + "\n\n")
	}
	b.WriteString(req.Lead + "\n")
	if req.Body != "" {
		b.WriteString("\n" + req.Body + "\n")
	}
	for _, f := range req.Fields {
		b.WriteString(f.Label + ": " + f.Value + "\n")
	}
	// Read-only actions: link URLs and server-minted links are shown as
	// labels, not keybound.
	for _, a := range req.Actions {
		switch a.Kind {
		case channelevents.ActionKindLink:
			b.WriteString(th.Render(th.Subtle, a.Label+": "+a.URL) + "\n")
		case channelevents.ActionKindLinkMint:
			b.WriteString(th.Render(th.Subtle, a.Label+" (respond from a connected app)") + "\n")
		}
	}
	// Decision actions become the modal's keybindings.
	hints := make([]string, 0, len(req.Actions))
	for _, bnd := range decisionBindings(req) {
		hints = append(hints, "["+bnd.Key+"] "+bnd.Label)
	}
	if len(hints) > 0 {
		b.WriteString("\n" + th.Render(th.Subtle, strings.Join(hints, "   ")))
	}
	inner := th.Frame.Width(width - 4).Render(b.String())
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, inner)
}
