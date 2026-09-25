// pkg/channels/channelkinds/slack/stream_delta_sink.go
//
// Slack stream-delta sink. Renders runner-emitted LLM stream events onto a
// sink-owned streaming bubble via debounced chat.update calls.
//
// State and debounce are per thread. The default window is 1.5s; a Slack 429
// doubles it for that thread (capped) until the next successful update, then it
// snaps back.
//
// Bubble lifecycle:
//
//	The listener posts an informational starter and walks away. On the first
//	flushable delta the sink lazily calls Poster.PostMessage to create the
//	"streaming bubble"; later deltas Updater.UpdateMessage that ts. A "stop"
//	clears the buffer and moves the ts into st.finalizedTS, which the sender
//	retrieves and clears via ConsumeFinalTarget before chat.updating it with
//	the polished final reply. An empty ConsumeFinalTarget means no streaming
//	occurred (DM/sidebar threads), and the sender posts fresh instead.
//
// The buffer is cumulative WITHIN a turn — each chat.update sends everything the
// agent has emitted so far — and is cleared on "stop".
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Updater is the minimal Slack-client surface the sink needs for editing an
// existing message. The kind's existing slackClient interface is a superset;
// we accept the narrower interface so unit tests can stub without dragging in
// the full SDK.
type Updater interface {
	UpdateMessage(ctx context.Context, channel, ts, text string) error
}

// Poster is the minimal Slack-client surface the sink needs for posting a new
// message into a thread. Injected by Kind; tests stub.
type Poster interface {
	PostMessage(ctx context.Context, channelID, threadTS, text string) (ts string, err error)
}

// StreamDeltaSinkConfig configures a StreamDeltaSink.
type StreamDeltaSinkConfig struct {
	// Updater performs the actual chat.update Slack call.
	Updater Updater

	// Poster creates a new threaded message (the streaming bubble). When nil,
	// the sink drops all deltas silently — no streaming surface to render onto.
	Poster Poster

	// DebounceWindow is the per-thread debounce. Defaults to 1.5s.
	DebounceWindow time.Duration

	// MaxDebounceWindow caps the post-429 doubled window. Defaults to 6s.
	MaxDebounceWindow time.Duration

	// ThreadRoot supplies the thread a session's stream bubble must live in when
	// the session's binding has no thread_ts yet — a restart-fork child, whose
	// link-back root is created on demand. Without it the sink would post the
	// bubble at channel level while the sender consumes it under the root, miss,
	// and post the reply a second time. Nil disables the lookup.
	ThreadRoot func(ctx context.Context, sess channelkinds.SessionInfo, channelID string) string
}

// NewStreamDeltaSink constructs a StreamDeltaSink. Both Updater and Poster
// are required; without them OnDelta returns nil silently (no rendering).
func NewStreamDeltaSink(cfg StreamDeltaSinkConfig) *StreamDeltaSink {
	if cfg.DebounceWindow <= 0 {
		cfg.DebounceWindow = 1500 * time.Millisecond
	}
	if cfg.MaxDebounceWindow <= 0 {
		cfg.MaxDebounceWindow = 6 * time.Second
	}
	return &StreamDeltaSink{
		cfg:     cfg,
		threads: map[string]*threadState{},
	}
}

// StreamDeltaSink is the Slack-flavored renderer.
type StreamDeltaSink struct {
	cfg StreamDeltaSinkConfig

	mu      sync.Mutex
	threads map[string]*threadState
	closed  bool
}

// threadState carries per-thread (channel, thread_ts) buffer + debounce timer.
type threadState struct {
	mu         sync.Mutex
	buf        strings.Builder
	flushTimer *time.Timer
	debounce   time.Duration
	channelID  string
	threadTS   string
	// session is "ns/name" for the AgentSession this thread renders, captured
	// for diagnostics when a flush (running in a timer goroutine with no
	// caller context) fails.
	session string
	// placeholder is the ts of the streaming bubble posted by the sink on the
	// first flushable delta of a turn. Empty until the first flush succeeds.
	placeholder string
	// finalizedTS is the streaming bubble ts after the "stop" event clears the
	// buffer. The sender retrieves it via ConsumeFinalTarget to chat.update the
	// bubble with the polished final reply.
	finalizedTS string
}

// Compile-time interface assertion.
var _ channelkinds.StreamDeltaSink = (*StreamDeltaSink)(nil)

// OnDelta consumes a single KindAssistantStreamDelta envelope and updates
// the per-thread buffer + schedules a debounced flush. Errors decoding the
// payload are returned; downstream chat.update failures are handled
// internally (rate-limited retries; non-rate-limit errors are dropped to
// avoid retry storms on a permanent failure).
func (s *StreamDeltaSink) OnDelta(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) error {
	var pl channelevents.AssistantStreamDeltaPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("slack stream-delta sink: unmarshal payload: %w", err)
	}

	channelID, threadTS := s.resolveThread(ctx, sess)
	if channelID == "" {
		return nil // nothing to render onto
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	key := channelID + "|" + threadTS
	st := s.getOrCreateThread(key, channelID, threadTS, sess.Namespace+"/"+sess.Name)

	st.mu.Lock()
	switch pl.EventType {
	case "text_delta":
		st.buf.WriteString(pl.Text)
	case "tool_use_start":
		if st.buf.Len() > 0 {
			st.buf.WriteString("\n")
		}
		st.buf.WriteString("🔧 calling `")
		st.buf.WriteString(pl.ToolName)
		st.buf.WriteString("`")
	case "tool_use_stop":
		// Reserved — future "✓ done" rendering. No buffer change for v1.
	case "stop":
		// Turn complete. Move the streaming bubble ts to finalizedTS so the
		// sender can retrieve it via ConsumeFinalTarget.
		if st.placeholder != "" {
			st.finalizedTS = st.placeholder
			st.placeholder = ""
		}
		st.buf.Reset()
		if st.flushTimer != nil {
			st.flushTimer.Stop()
			st.flushTimer = nil
		}
		st.mu.Unlock()
		return nil
	default:
		// Unknown event type — drop silently.
	}
	s.scheduleFlushLocked(st)
	st.mu.Unlock()
	return nil
}

// ConsumeFinalTarget returns the streaming bubble ts for this thread's
// most-recently-finalized stream, then clears it. Returns "" if there was no
// streaming for this thread/turn (the sender then posts fresh). Safe to call
// from a different goroutine than the streaming flush.
func (s *StreamDeltaSink) ConsumeFinalTarget(channelID, threadTS string) string {
	key := channelID + "|" + threadTS
	s.mu.Lock()
	st, ok := s.threads[key]
	s.mu.Unlock()
	if !ok {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	ts := st.finalizedTS
	st.finalizedTS = ""
	return ts
}

// getOrCreateThread returns the threadState for key, creating one on miss.
func (s *StreamDeltaSink) getOrCreateThread(key, channelID, threadTS, session string) *threadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.threads[key]
	if !ok {
		st = &threadState{
			channelID: channelID,
			threadTS:  threadTS,
			session:   session,
			debounce:  s.cfg.DebounceWindow,
		}
		s.threads[key] = st
	}
	return st
}

// scheduleFlushLocked schedules a flush if one isn't already scheduled.
// Caller MUST hold st.mu.
func (s *StreamDeltaSink) scheduleFlushLocked(st *threadState) {
	if st.flushTimer != nil {
		return
	}
	debounce := st.debounce
	st.flushTimer = time.AfterFunc(debounce, func() { s.flush(st) })
}

// flush is the timer callback. On the first call when no streaming bubble
// exists yet, it calls Poster.PostMessage to create one. On subsequent calls
// it calls Updater.UpdateMessage on the existing bubble. A Slack 429 doubles
// the debounce window and reschedules; other errors are logged and dropped
// (no reschedule) so a permanent failure can't spin a retry storm.
func (s *StreamDeltaSink) flush(st *threadState) {
	st.mu.Lock()
	text := st.buf.String()
	channelID := st.channelID
	threadTS := st.threadTS
	session := st.session
	placeholder := st.placeholder
	st.flushTimer = nil
	st.mu.Unlock()

	if text == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if placeholder == "" {
		// First flush for this turn: lazily post the streaming bubble.
		if s.cfg.Poster == nil {
			return // no poster configured; drop silently (DM / no-token path)
		}
		ts, err := s.cfg.Poster.PostMessage(ctx, channelID, threadTS, text)

		st.mu.Lock()
		defer st.mu.Unlock()
		if err != nil {
			// flush runs in a time.AfterFunc goroutine with no caller context.
			log.FromContext(context.Background()).Info("slack stream-delta sink: PostMessage (first-flush bubble) failed",
				"session", session, "channel", channelID, "threadTS", threadTS, "err", err.Error())
			if isRateLimited(err) {
				// Rate-limited: back off and reschedule — the bubble can still post.
				next := st.debounce * 2
				if next > s.cfg.MaxDebounceWindow {
					next = s.cfg.MaxDebounceWindow
				}
				st.debounce = next
				s.scheduleFlushLocked(st)
				return
			}
			// Non-rate-limit (permanent) error: do NOT reschedule. Retrying on
			// every subsequent delta would loop forever with zero progress; the
			// final user_message will surface a fresh chat.postMessage instead.
			return
		}
		// Bubble posted successfully; record it for subsequent updates.
		st.placeholder = ts
		st.debounce = s.cfg.DebounceWindow
		return
	}

	// Streaming bubble already exists: update it in place.
	err := s.cfg.Updater.UpdateMessage(ctx, channelID, placeholder, text)

	st.mu.Lock()
	defer st.mu.Unlock()
	if err == nil {
		// Reset to configured debounce on success. Subsequent deltas will
		// re-schedule with the fresh window.
		st.debounce = s.cfg.DebounceWindow
		return
	}
	if isRateLimited(err) {
		// Double the window, capped at MaxDebounceWindow. Reschedule a
		// retry — the buffer is unchanged (we never reset on flush) so
		// the retry sends the same cumulative text plus any new deltas
		// that landed since.
		next := st.debounce * 2
		if next > s.cfg.MaxDebounceWindow {
			next = s.cfg.MaxDebounceWindow
		}
		st.debounce = next
		s.scheduleFlushLocked(st)
		return
	}
	// Non-rate-limit error: drop. A permanent failure (streaming bubble deleted,
	// missing scope, etc.) shouldn't trigger an infinite retry loop. The
	// final user_message will surface a fresh chat.postMessage if the
	// chat.update path stays broken. Log before dropping so the degraded
	// streaming surface is diagnosable. flush runs in a time.AfterFunc
	// goroutine with no caller context, hence Background.
	log.FromContext(context.Background()).Info("slack stream-delta sink: UpdateMessage failed; dropping (no retry)",
		"session", session, "channel", channelID, "ts", placeholder, "err", err.Error())
}

// isRateLimited reports whether err looks like a Slack rate-limit response.
// slack-go surfaces rate_limited as either a literal "rate_limited" error
// string or a *slackapi.RateLimitedError; we accept both.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var rl *slackapi.RateLimitedError
	if errors.As(err, &rl) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "rate_limited") || strings.Contains(msg, "429")
}

// Close stops all pending timers. Idempotent.
func (s *StreamDeltaSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for _, st := range s.threads {
		st.mu.Lock()
		if st.flushTimer != nil {
			st.flushTimer.Stop()
			st.flushTimer = nil
		}
		st.mu.Unlock()
	}
}

// resolveThread reads channel_id from the SessionInfo's ChannelBinding.External
// map and resolves thread_ts via effectiveOutboundThreadTS (the per-turn
// LastInboundTS annotation, falling back to External["thread_ts"]). When the
// resolved thread_ts is empty — a restart-fork child whose binding has no
// thread yet — and cfg.ThreadRoot is configured, it asks ThreadRoot for the
// (idempotent, cached) link-back root so the bubble is keyed and posted under
// the SAME root the sender's fork-root guard resolves; without this the sink
// would key the bubble under an empty thread_ts while the sender consults
// ConsumeFinalTarget under the root, missing and posting a duplicate reply.
// Returns ("", "") if the binding is missing the channel id (in which case
// OnDelta drops the event).
func (s *StreamDeltaSink) resolveThread(ctx context.Context, sess channelkinds.SessionInfo) (channelID, threadTS string) {
	if sess.Channel == nil || sess.Channel.External == nil {
		return "", ""
	}
	channelID = sess.Channel.External["channel_id"]
	threadTS = effectiveOutboundThreadTS(sess)
	if threadTS == "" && s.cfg.ThreadRoot != nil && channelID != "" {
		threadTS = s.cfg.ThreadRoot(ctx, sess, channelID)
	}
	return channelID, threadTS
}

// SlackUpdater wraps slack-go's chat.update via the existing slackClient
// interface so the sink's narrower Updater can plug into the real client
// without callers reaching into slackapi.MsgOption directly.
type SlackUpdater struct {
	Client slackClient
}

// UpdateMessage calls UpdateMessageContext with a single MsgOptionText. The
// `false` second arg to MsgOptionText preserves the LLM's literal mrkdwn
// (matching the Sender's chat.update path on the streaming bubble).
func (a *SlackUpdater) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	if a == nil || a.Client == nil {
		return fmt.Errorf("slack stream-delta sink: client not configured")
	}
	_, _, _, err := a.Client.UpdateMessageContext(ctx, channel, ts, slackapi.MsgOptionText(text, false))
	return err
}

// SlackPoster wraps slack-go's chat.postMessage so the sink can post new
// streaming bubbles into a thread without callers reaching into MsgOption.
type SlackPoster struct {
	Client slackClient
}

// PostMessage posts text as a threaded reply under threadTS in channelID
// and returns the new message's ts. Returns an error if the client is not
// configured or the API call fails.
func (p *SlackPoster) PostMessage(ctx context.Context, channelID, threadTS, text string) (string, error) {
	if p == nil || p.Client == nil {
		return "", fmt.Errorf("slack stream-delta sink: poster client not configured")
	}
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	_, ts, err := p.Client.PostMessageContext(ctx, channelID, opts...)
	return ts, err
}

// NewSlackUpdaterFromSecret constructs the production SlackUpdater wired
// to slack-go's *slackapi.Client built from the bot-token in the Secret.
// Returns nil if the Secret is missing or malformed; callers should treat
// nil as "kind opts out" (the sink will no-op on every delta).
func NewSlackUpdaterFromSecret(deps channelkinds.Deps) *SlackUpdater {
	c := newSlackAPIClient(deps.Secret)
	if c == nil {
		return nil
	}
	return &SlackUpdater{Client: c}
}

// NewSlackPosterFromSecret constructs the production SlackPoster wired to
// slack-go's *slackapi.Client built from the bot-token in the Secret.
// Returns nil if the Secret is missing or malformed.
func NewSlackPosterFromSecret(deps channelkinds.Deps) *SlackPoster {
	c := newSlackAPIClient(deps.Secret)
	if c == nil {
		return nil
	}
	return &SlackPoster{Client: c}
}
