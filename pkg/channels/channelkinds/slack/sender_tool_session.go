// Package slack — tool_session sub-channel sender.
//
// Each tool run (one ToolCallRef) is rendered as a single Block Kit
// message: a header, a status/elapsed line, and a fenced code block of
// the streamed output. The message is posted on the first event and
// edited in place by a hybrid timer — a ~2s debounce coalesces event
// bursts, a ~5s fallback tick advances the elapsed clock during quiet
// stretches. The message is finalized (and the cache entry dropped) on
// the `result` event / Terminal delta. Mirrors stream_delta_sink.go's
// time.AfterFunc + 429-backoff pattern.

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

const (
	defaultToolSessionDebounce = 2 * time.Second
	defaultToolSessionFallback = 5 * time.Second
	maxToolSessionBackoff      = 10 * time.Second
)

// toolSessionRef is the per-ToolCallRef Slack-side state. Its mutex
// guards every field; the flush timer callback runs in its own goroutine.
type toolSessionRef struct {
	mu         sync.Mutex
	cli        slackClient
	channelID  string
	msgTS      string
	threadTS   string
	session    string // "ns/name" — for diagnostics on a failed chat.update
	toolName   string
	reason     string
	startedAt  time.Time
	transcript strings.Builder
	finished   bool
	ok         bool
	costUSD    float64
	flushTimer *time.Timer
	backoff    time.Duration
}

// stop cancels the flush timer. Safe to call repeatedly.
func (r *toolSessionRef) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flushTimer != nil {
		r.flushTimer.Stop()
		r.flushTimer = nil
	}
}

// viewLocked snapshots the ref into a renderable toolSessionView.
// Caller holds r.mu.
func (r *toolSessionRef) viewLocked(now time.Time) toolSessionView {
	return toolSessionView{
		toolName:   r.toolName,
		reason:     r.reason,
		transcript: r.transcript.String(),
		elapsed:    now.Sub(r.startedAt),
		finished:   r.finished,
		ok:         r.ok,
		costUSD:    r.costUSD,
	}
}

type toolSessionRefCache struct {
	mu sync.Mutex
	m  map[string]*toolSessionRef
}

func newToolSessionRefCache() *toolSessionRefCache {
	return &toolSessionRefCache{m: map[string]*toolSessionRef{}}
}

func (c *toolSessionRefCache) get(id string) *toolSessionRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.m[id]
	if !ok {
		r = &toolSessionRef{}
		c.m[id] = r
	}
	return r
}

func (c *toolSessionRefCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

// has reports whether a live ref exists for id — a non-creating lookup.
func (c *toolSessionRefCache) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[id]
	return ok
}

// toolSessionSeen marks sessions that have streamed tool-session output
// this process. The Slack message sender consults it: once a session
// has streamed tool output, the final agent reply is posted fresh
// (appended below the tool-session messages) rather than edited into
// the listener placeholder, which sits above them.
type toolSessionSeen struct {
	mu sync.Mutex
	m  map[string]bool
}

func newToolSessionSeen() *toolSessionSeen {
	return &toolSessionSeen{m: map[string]bool{}}
}

func (s *toolSessionSeen) mark(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[session] = true
}

func (s *toolSessionSeen) saw(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[session]
}

type toolSessionSender struct {
	deps          channelkinds.Deps
	cli           slackClient
	refs          *toolSessionRefCache
	seen          *toolSessionSeen
	debounceWin   time.Duration // default 2s; tests shorten
	fallbackEvery time.Duration // default 5s; tests shorten
	// forkRoot creates a restart-fork child's thread root exactly once,
	// shared with slackSender so whichever one reaches the channel first
	// wins. Injected by Kind; nil in direct-construction tests, treated as
	// no fork children.
	forkRoot *forkRootCache
}

func newToolSessionSender(deps channelkinds.Deps) *toolSessionSender {
	return &toolSessionSender{
		deps:          deps,
		cli:           newSlackAPIClient(deps.Secret),
		refs:          newToolSessionRefCache(),
		debounceWin:   defaultToolSessionDebounce,
		fallbackEvery: defaultToolSessionFallback,
	}
}

// Send dispatches one tool_session envelope. KindToolSessionEvent
// (parsed) and KindToolSessionDelta (raw) share the per-ToolCallRef
// Block Kit message.
func (s *toolSessionSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if s.cli == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack tool_session: bot-token missing from credentials Secret")
	}
	switch env.Kind {
	case channelevents.KindToolSessionEvent:
		return s.handleEvent(ctx, sess, env)
	case channelevents.KindToolSessionDelta:
		return s.handleDelta(ctx, sess, env)
	default:
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack tool_session: unexpected kind %q", env.Kind)
	}
}

func (s *toolSessionSender) handleEvent(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	var pl channelevents.ToolSessionEventPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack tool_session: unmarshal event payload: %w", err)
	}
	if pl.ToolCallRef == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack tool_session: empty ToolCallRef")
	}
	in := ingest{
		// The header names the dispatched (outer) tool, e.g. "claude" —
		// not pl.ToolName, which on tool_use events is the streaming
		// agent's own internal tool (Write, Read, …). Those still render
		// inside the code block via eventTranscriptLine.
		toolName: pl.OuterTool,
		reason:   pl.Reason,
		append:   eventTranscriptLine(pl),
		finished: pl.EventType == "result",
		ok:       pl.OK,
		costUSD:  pl.CostUSD,
	}
	return s.apply(ctx, sess, pl.ToolCallRef, in)
}

func (s *toolSessionSender) handleDelta(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	var pl channelevents.ToolSessionDeltaPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack tool_session: unmarshal delta payload: %w", err)
	}
	if pl.ToolCallRef == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack tool_session: empty ToolCallRef")
	}
	// A terminal delta with no data for a ToolCallRef we hold no live
	// message for is a redundant end-signal — typically the OnTerminal
	// raw delta that trails a parsed `result` event, which already
	// finalized and dropped the message. Posting for it would just leave
	// a spurious empty "(no output yet)" tool-session message.
	if pl.Terminal && len(pl.Data) == 0 && !s.refs.has(pl.ToolCallRef) {
		return channelkinds.SubChannelSendResult{}, nil
	}
	in := ingest{
		append:   string(pl.Data), // raw path: verbatim stdout
		finished: pl.Terminal,
		ok:       pl.Terminal && (pl.ExitReason == "completed" || pl.ExitCode == 0),
	}
	return s.apply(ctx, sess, pl.ToolCallRef, in)
}

// ingest is the kind-agnostic update applied to a toolSessionRef.
type ingest struct {
	toolName string
	reason   string
	append   string
	finished bool
	ok       bool
	costUSD  float64
}

// eventTranscriptLine renders one parsed event into a code-block line.
func eventTranscriptLine(pl channelevents.ToolSessionEventPayload) string {
	switch pl.EventType {
	case "text_delta":
		return pl.Text
	case "tool_use_start":
		name := pl.ToolName
		if pl.Summary != "" {
			if name != "" {
				return "\n→ " + name + " " + pl.Summary + "\n"
			}
			return "\n→ " + pl.Summary + "\n"
		}
		return "\n→ " + name + "\n"
	case "tool_use_stop":
		mark := "✓"
		if !pl.OK {
			mark = "✗"
		}
		if pl.Summary != "" {
			return "  " + mark + " " + pl.Summary + "\n"
		}
		return "  " + mark + "\n"
	case "result":
		return "" // result drives the status line, not the code block
	default:
		return ""
	}
}

// apply mutates the ref, posts the message on first contact, and
// schedules (or, on finish, forces) a flush.
func (s *toolSessionSender) apply(ctx context.Context, sess channelkinds.SessionInfo, toolCallRef string, in ingest) (channelkinds.SubChannelSendResult, error) {
	channelID, threadTS := resolveChannelAndThread(sess)
	if channelID == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack tool_session: no channel_id in session External map")
	}
	// A fork child has no thread yet. Create it with the link-back framing
	// message so the first thing anyone reads is where this came from — not a
	// streaming tool bubble.
	if threadTS == "" && s.forkRoot != nil {
		if root := s.forkRoot.ensure(ctx, s.cli, s.deps.K8sClient, sess, channelID); root != "" {
			threadTS = root
		}
	}
	ref := s.refs.get(toolCallRef)

	ref.mu.Lock()
	first := ref.msgTS == "" && ref.startedAt.IsZero()
	if first {
		ref.cli = s.cli
		ref.channelID = channelID
		ref.threadTS = threadTS
		ref.session = sess.Namespace + "/" + sess.Name
		ref.startedAt = time.Now()
	}
	if in.toolName != "" {
		ref.toolName = in.toolName
	}
	if in.reason != "" {
		ref.reason = in.reason
	}
	if in.append != "" {
		ref.transcript.WriteString(in.append)
	}
	if in.finished {
		ref.finished = true
		ref.ok = in.ok
		if in.costUSD > 0 {
			ref.costUSD = in.costUSD
		}
	}
	ref.mu.Unlock()

	if first {
		// Record that this session streamed tool output so the final
		// agent reply appends below it instead of editing the listener
		// placeholder, which sits above the tool-session messages.
		if s.seen != nil {
			s.seen.mark(sess.Namespace + "/" + sess.Name)
		}
		if err := s.post(ctx, ref); err != nil {
			return channelkinds.SubChannelSendResult{}, err
		}
	}
	if in.finished {
		s.finalize(ctx, toolCallRef, ref)
	} else {
		// Debounce: a burst of events coalesces into one flush ~2s after
		// the last. The first event arms the timer so the elapsed clock
		// starts ticking (the flush re-arms a ~5s fallback after each).
		s.schedule(ref, s.debounceWin)
	}
	// Snapshot the identifiers under the lock — post() writes ref.msgTS
	// from its own lock acquire, and a concurrent apply() for the same
	// ToolCallRef must not read these fields unsynchronized.
	ref.mu.Lock()
	// Report the THREAD the bubble lives in, not the bubble's own message ts.
	// For a fork child the enclosing thread is the link-back root that ensure()
	// created; the relay patches the session's binding + LabelChannelKey from
	// this value, and a user's later reply in that thread carries the ROOT ts.
	// When there is no enclosing thread (an ordinary channel-level first send)
	// the bubble itself IS the root.
	rootTS := ref.threadTS
	if rootTS == "" {
		rootTS = ref.msgTS
	}
	res := channelkinds.SubChannelSendResult{
		External: map[string]string{"channel_id": ref.channelID, "thread_ts": rootTS},
	}
	ref.mu.Unlock()
	return res, nil
}

// post does the first chat.postMessage and records the ts.
func (s *toolSessionSender) post(ctx context.Context, ref *toolSessionRef) error {
	ref.mu.Lock()
	blocks := renderToolSessionBlocks(ref.viewLocked(time.Now()))
	channelID, threadTS := ref.channelID, ref.threadTS
	ref.mu.Unlock()

	opts := []slackapi.MsgOption{slackapi.MsgOptionBlocks(blocks...)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	_, ts, err := s.cli.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return fmt.Errorf("slack tool_session: PostMessage: %w", err)
	}
	ref.mu.Lock()
	ref.msgTS = ts
	ref.mu.Unlock()
	return nil
}

// schedule (re)arms the flush timer at delay. A new event resets a
// pending timer back to the debounce window. Caller must not hold ref.mu.
func (s *toolSessionSender) schedule(ref *toolSessionRef, delay time.Duration) {
	ref.mu.Lock()
	defer ref.mu.Unlock()
	if ref.finished {
		return
	}
	if ref.flushTimer != nil {
		ref.flushTimer.Stop()
	}
	ref.flushTimer = time.AfterFunc(delay, func() { s.flush(ref) })
}

// flush renders + chat.updates the message, then re-arms the ~5s
// fallback tick so the elapsed clock advances while idle. On a Slack
// 429 it backs off and retries.
//
// Timer-slot discipline: flush nulls ref.flushTimer at its start, so a
// schedule() called by a concurrent event during the chat.update HTTP
// call re-arms a sooner (~2s) flush. At the end flush only re-arms the
// fallback if that slot is still empty — the sooner debounce wins.
func (s *toolSessionSender) flush(ref *toolSessionRef) {
	ref.mu.Lock()
	ref.flushTimer = nil
	if ref.finished || ref.msgTS == "" {
		ref.mu.Unlock()
		return
	}
	blocks := renderToolSessionBlocks(ref.viewLocked(time.Now()))
	cli, channelID, ts, session := ref.cli, ref.channelID, ref.msgTS, ref.session
	ref.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _, err := cli.UpdateMessageContext(ctx, channelID, ts, slackapi.MsgOptionBlocks(blocks...))

	ref.mu.Lock()
	defer ref.mu.Unlock()
	if ref.finished {
		return // finalize() owns the terminal update
	}
	switch {
	case err == nil:
		ref.backoff = 0
		if ref.flushTimer == nil { // no event re-armed a sooner flush
			ref.flushTimer = time.AfterFunc(s.fallbackEvery, func() { s.flush(ref) })
		}
	case isRateLimited(err):
		next := ref.backoff * 2
		if next < s.debounceWin {
			next = s.debounceWin
		}
		if next > maxToolSessionBackoff {
			next = maxToolSessionBackoff
		}
		ref.backoff = next
		if ref.flushTimer == nil {
			ref.flushTimer = time.AfterFunc(next, func() { s.flush(ref) })
		}
	default:
		// Non-rate-limit error: log + don't retry (no storm). The tool
		// still runs; only its Slack mirror degrades. flush runs in a
		// time.AfterFunc goroutine with no caller context, hence Background.
		log.FromContext(context.Background()).Info("slack tool_session: chat.update failed",
			"session", session, "channel", channelID, "err", err.Error())
	}
}

// finalize stops the timer, does one synchronous final chat.update, and
// drops the cache entry.
func (s *toolSessionSender) finalize(ctx context.Context, toolCallRef string, ref *toolSessionRef) {
	ref.stop()
	ref.mu.Lock()
	blocks := renderToolSessionBlocks(ref.viewLocked(time.Now()))
	cli, channelID, ts, session := ref.cli, ref.channelID, ref.msgTS, ref.session
	ref.mu.Unlock()
	if ts != "" {
		if _, _, _, err := cli.UpdateMessageContext(ctx, channelID, ts, slackapi.MsgOptionBlocks(blocks...)); err != nil {
			log.FromContext(ctx).Info("slack tool_session: final chat.update failed",
				"session", session, "channel", channelID, "toolCallRef", toolCallRef, "err", err.Error())
		}
	}
	s.refs.drop(toolCallRef)
}
