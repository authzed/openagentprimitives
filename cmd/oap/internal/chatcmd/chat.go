// Package chatcmd implements `oap agent chat <agent-class>`: a full-screen
// interactive TUI peer of the Slack channel. It creates an ephemeral `local`
// Channel CR + a channel-attached AgentSession, submits inbound messages to
// channelsd over NATS request-reply (the same view_message path
// pkg/web/webui/chat uses) and runs the outbound relay locally (against oap's
// port-forwarded clients), driving the bubbletea TUI in tui.go. The runner /
// operator / channelsd core are unchanged; channelsd skips the local Channel
// via RelayedByChannelsd(). oap never appends to session memory itself — see
// waitForSessionReady.
package chatcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clinats"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	local "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

func NewCmd(g *apcmd.Globals) *cobra.Command {
	var (
		sessionName    string
		budgetTokens   int64
		budgetTurns    int32
		budgetDuration time.Duration
	)
	cmd := &cobra.Command{
		Use:   "chat <agent-class>",
		Short: "Drive an AgentClass agent through an interactive full-screen TUI",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !apcmd.StdinIsInteractive(os.Stdin) {
				return fmt.Errorf("`oap agent chat` needs an interactive terminal; " +
					"for non-interactive / scripted runs use `oap agent run`")
			}
			return runAgentChat(cmd.Context(), g, args[0],
				sessionName, budgetTokens, budgetTurns, budgetDuration)
		},
	}
	apcmd.SessionNameFlag(cmd, &sessionName)
	cmd.Flags().Int64Var(&budgetTokens, "budget-tokens", 0, "Per-session max tokens")
	cmd.Flags().Int32Var(&budgetTurns, "budget-turns", 0, "Per-session max turns")
	cmd.Flags().DurationVar(&budgetDuration, "budget-duration", 0, "Per-session max wall-time")
	return cmd
}

// buildChatChannel constructs the ephemeral `local` Channel CR. It is
// owned by the AgentClass so a hard `oap` crash cannot orphan it.
func buildChatChannel(ns, sessionName, agentClass string, ac *spiceboxv1alpha1.AgentClass) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "Channel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionName + "-chan",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentClass",
				Name:       ac.Name,
				UID:        ac.UID,
			}},
		},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "local",
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:     agentClass,
			SessionScope:   "user",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: sessionName + "-chan-creds"},
		},
	}
}

// buildChatSession constructs the channel-attached AgentSession with the
// pipeline's correlation labels (channel.name + sha256(channelKey)) so a
// typed message routes to this pre-created session, plus the
// InputChannel binding. Owned by the Channel CR for cleanup.
//
// startedByExternalID must be the ExternalID from the ExternalIdentity that
// the local listener will stamp on inbound events (chatExternalIdentity's
// output). It is stamped on AnnotationStartedByExternalID so
// approverIsStartedBy (pkg/channels/channelsd/pipeline/decision.go) can match an
// approval click back to this session. channelsd-created sessions get this
// annotation from pipeline.Deliver's new-session path; `oap agent chat`
// pre-creates the session itself so it must stamp the annotation here.
// Without it, every approval click on a chat session is rejected.
func buildChatSession(ns, sessionName, agentClass string, ch *spiceboxv1alpha1.Channel, prompt, startedByExternalID, startedByEmail string) *spiceboxv1alpha1.AgentSession {
	key := local.ChannelKey(ns, ch.Name)
	return &spiceboxv1alpha1.AgentSession{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "AgentSession",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionName,
			Namespace: ns,
			Labels: map[string]string{
				// These three labels are the channelKey→session correlation
				// index the pipeline's Deliver matches on. LabelChannelKey
				// MUST be stamped with channelkey.LabelValue — the
				// EXACT same function the pipeline uses to LOOK UP the
				// session and to stamp the label on sessions it creates
				// itself. A divergent hash here routes follow-up messages
				// to a brand-new bogus session (whose memory token doesn't
				// authorize the user → 403). See bug #5.
				spiceboxv1alpha1.LabelChannelName: ch.Name,
				spiceboxv1alpha1.LabelChannelKind: local.KindName,
				spiceboxv1alpha1.LabelChannelKey:  channelkey.LabelValue(key),
			},
			Annotations: map[string]string{
				// User's kind-specific external ID — the same value
				// channelsd's pipeline writes for slack/fake sessions.
				// approverIsStartedBy matches an approval click against
				// this; the inbound permission deny path also reads it.
				spiceboxv1alpha1.AnnotationStartedByExternalID: startedByExternalID,
				// Verified email of the initiator, when the identity is
				// email-bearing. identity_choice's DecideRequester check
				// canonicalizes the requester by email, so an email-less
				// stamp leaves a CLI initiator unable to decide their own
				// identity prompt (fail-closed) — mirrors the webchat and
				// channelsd session-creation paths.
				spiceboxv1alpha1.AnnotationStartedByEmail: startedByEmail,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "Channel",
				Name:       ch.Name,
				UID:        ch.UID,
			}},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  agentClass,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: prompt},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:         ch.Name,
				Kind:         local.KindName,
				Key:          key,
				Capabilities: (&local.Kind{}).Capabilities(),
				// channelevents.SubjectPrefix is THE function the pipeline
				// (makeSessionName→SubjectPrefix), the runner (publishes
				// outbound on SubjectOut(prefix,...)), and outbound.Relay
				// (subscribes ap.session.*.*.out.>) all agree on. Hardcoding
				// the string would silently drift from that contract.
				NATSSubjectPrefix: channelevents.SubjectPrefix(ns, sessionName),
			},
		},
	}
}

// runAgentChat is the lifecycle: resolve AgentClass → create ephemeral
// Channel + creds Secret → port-forward NATS/SpiceDB → run the bubbletea
// program. The AgentSession is NOT created at startup — the runner
// requires a non-empty spec.prompt, so the session is created on the
// user's FIRST message (with that message as spec.prompt.inline,
// mirroring how Slack starts a session from its first inbound). Only
// then are the session-scoped pieces (a readiness wait, the local host,
// outbound relay) constructed. Cleanup is best-effort; owner references
// guarantee a hard crash cannot orphan the Channel or session.
func runAgentChat(ctx context.Context, g *apcmd.Globals, agentClass, sessionName string,
	budgetTokens int64, budgetTurns int32, budgetDuration time.Duration) error {

	// Resolve the CLI identity FIRST — the interactive login flow may run
	// here, and it must complete before we create any cluster resources.
	// Login failure is a hard error: there is no silent fallback once we
	// have resources to clean up (the deferred teardown fires regardless).
	principal, err := clilogin.EnsureIdentity(ctx, g)
	if err != nil {
		return fmt.Errorf("resolve identity: %w", err)
	}

	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// 1. Resolve the AgentClass; require Valid=True.
	var ac spiceboxv1alpha1.AgentClass
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: agentClass}, &ac); err != nil {
		return fmt.Errorf("get AgentClass %q: %w", agentClass, err)
	}
	if !apcmd.AgentClassIsValid(&ac) {
		return fmt.Errorf("AgentClass %q is not Valid=True yet", agentClass)
	}

	if sessionName == "" {
		sessionName = fmt.Sprintf("%s-%s", agentClass, uuid.New().String()[:8])
	}

	// 2. Create the ephemeral local Channel CR + its (empty) creds Secret.
	ch := buildChatChannel(b.Namespace, sessionName, agentClass, &ac)
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ch.Spec.CredentialsRef.SecretName, Namespace: b.Namespace},
	}
	if err := b.Controller.Create(ctx, creds); err != nil {
		return fmt.Errorf("create chat creds Secret: %w", err)
	}
	if err := b.Controller.Create(ctx, ch); err != nil {
		return fmt.Errorf("create local Channel: %w", err)
	}
	defer func() {
		// Best-effort teardown; owner refs are the durable guarantee.
		// The Channel + creds always exist by here, so delete them
		// unconditionally.
		_ = b.Controller.Delete(context.Background(), ch)
		_ = b.Controller.Delete(context.Background(), creds)
	}()
	// Re-Get the Channel so ch.UID is populated for the session's owner ref.
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ch.Name}, ch); err != nil {
		return fmt.Errorf("re-get local Channel for UID: %w", err)
	}

	// 3. Port-forward NATS (:4222), SpiceDB (:50051). These are
	// session-independent and can come up before any message. `oap` does
	// NOT port-forward the operator's memory API: it never appends to
	// session memory itself (see waitForSessionReady), so it has no need
	// of a memory-writing client.
	natsPF, err := portforward.New(b.REST, "agentprimitives-system", "spicebox-nats",
		"app.kubernetes.io/name=spicebox-nats", 4222, 0)
	if err != nil {
		return err
	}
	if err := natsPF.Start(ctx, io.Discard); err != nil {
		return fmt.Errorf("port-forward NATS: %w", err)
	}
	defer natsPF.Stop()

	azClient, azCleanup, err := apspicedb.DialViaPortForward(ctx, b)
	if err != nil {
		return fmt.Errorf("connect SpiceDB: %w", err)
	}
	defer azCleanup()

	// 4. NATS connection (session-independent), authenticated with the
	// CLI's per-client NATS creds + TLS.
	natsOpts, natsCleanup, err := clinats.LoadCreds(ctx, b)
	if err != nil {
		return err
	}
	defer natsCleanup()
	nc, err := dialNATSForChat(natsOpts, natsPF.LocalPort())
	if err != nil {
		return err
	}
	defer nc.Drain() //nolint:errcheck

	// 5. Build the bubbletea program + model. The session-scoped pieces
	// (memory, pipeline, host, relay) are constructed lazily on the first
	// user message — see chatSessionStarter below.
	//
	// There is a genuine construction cycle: the chatModel needs its
	// submit/decide callbacks wired BEFORE it is handed to
	// tea.NewProgram (the model is a value type — tea.NewProgram boxes a
	// COPY, so any field assigned afterwards never reaches the program's
	// model); but the callbacks live on the chatSessionStarter, which
	// needs the *tea.Program to call program.Send. The cycle is broken
	// by binding the callbacks to the *chatSessionStarter POINTER while
	// its program field is still nil, then backfilling starter.program
	// once tea.NewProgram has returned. By the time submit/decide are
	// actually invoked (user input, after program.Run starts the Update
	// loop) starter.program is set.
	starter := &chatSessionStarter{
		ctx:            ctx,
		b:              b,
		ch:             ch,
		ac:             &ac,
		agentClass:     agentClass,
		sessionName:    sessionName,
		principal:      principal,
		nc:             nc,
		azClient:       azClient,
		budgetTokens:   budgetTokens,
		budgetTurns:    budgetTurns,
		budgetDuration: budgetDuration,
	}
	defer starter.teardown()

	// buildWiredChatModel is called inline as the tea.NewProgram argument
	// so there is no place to "wire the callbacks after" — the model
	// tea.NewProgram copies ALREADY has non-nil submit/decide. The
	// callbacks are method values bound to the starter pointer, so the
	// later starter.program backfill below is visible to them.
	program := tea.NewProgram(
		buildWiredChatModel(agentClass, sessionName, g.NoColor, starter),
		tea.WithContext(ctx), tea.WithAltScreen(),
	)

	// 6. Backfill the program onto the starter now that it exists. This
	// happens before program.Run() — and submit/decide are only invoked
	// from the Update loop program.Run() drives — so starter.program is
	// always set by the time a callback fires.
	starter.setProgram(program)

	// 7. Run the program to completion (q / ctrl-c quits).
	_, runErr := program.Run()
	return runErr
}

// buildWiredChatModel constructs a chatModel with its submit/decideInteraction
// callbacks ALREADY wired to the starter. Because chatModel is a value
// type, tea.NewProgram boxes a copy of whatever it is handed — so the
// callbacks MUST be set before that copy is made. Calling this inline as
// the tea.NewProgram argument makes "wire after" structurally
// impossible. The callbacks are method values bound to the
// *chatSessionStarter pointer, so a later starter.program assignment is
// visible to them when they run.
func buildWiredChatModel(agentClass, sessionName string, noColor bool, starter *chatSessionStarter) chatModel {
	model := newChatModel(agentClass, sessionName, noColor)
	model.submit = starter.submit
	model.decideInteraction = starter.decideInteraction
	return model
}

// chatSessionStarter owns the deferred, one-time construction of the
// AgentSession and all session-scoped infrastructure (a readiness wait,
// the local host, outbound relay). The runner requires a non-empty
// spec.prompt, so the session is created on the user's first message
// with that message as the inline prompt.
type chatSessionStarter struct {
	ctx context.Context
	b   *kube.Bundle

	ch          *spiceboxv1alpha1.Channel
	ac          *spiceboxv1alpha1.AgentClass
	agentClass  string
	sessionName string
	// principal is the resolved CLI identity for this chat session.
	// Set once at startup by runAgentChat via clilogin.EnsureIdentity; used in
	// startSession to stamp the started_by canonical and as the User on
	// the local host listener. Immutable after construction.
	principal identity.Principal

	nc       *nats.Conn
	azClient *spicedb.Client

	budgetTokens   int64
	budgetTurns    int32
	budgetDuration time.Duration

	mu sync.Mutex
	// program is backfilled by runAgentChat after tea.NewProgram returns
	// (a construction cycle — see runAgentChat). It is written once on
	// the main goroutine before program.Run(), and read in the goroutines
	// submit/decideInteraction spawn (which only run from the Update loop
	// program.Run drives). All access goes through the mutex so the write/read
	// pair is race-free regardless of the bubbletea internals' happens-before.
	program *tea.Program
	started bool                           // first message already handled
	sess    *spiceboxv1alpha1.AgentSession // non-nil once created
	host    *local.Host
	relay   *outbound.Relay
}

// hostDeps builds the channelkinds.Deps the TUI's local host serves every
// sender from. Split out of startSession so the minter assignments are
// reachable by a test: startSession itself needs a running bubbletea program,
// so nothing there is testable, and a minter dropped from this literal is
// otherwise caught only by the compiler noticing an unused variable — which
// stops being true the moment the variable has a second reader.
//
// Each minter is passed in rather than built here because each is
// best-effort: startSession decides what to tell the user about a builder
// that declined, and a nil minter is a normal state the senders handle.
func (s *chatSessionStarter) hostDeps(
	artifactMinter channelkinds.ArtifactViewMinter,
	sessionViewMinter channelkinds.SessionViewMinter,
	agentUIMinter channelkinds.AgentUIMinter,
) channelkinds.Deps {
	return channelkinds.Deps{
		Channel:     s.ch,
		NATSPublish: func(subj string, p []byte) error { return s.nc.Publish(subj, p) },
		NATSRequest: func(subj string, p []byte, timeout time.Duration) ([]byte, error) {
			msg, err := s.nc.Request(subj, p, timeout)
			if err != nil {
				return nil, err
			}
			return msg.Data, nil
		},
		K8sClient:          s.b.Controller,
		ArtifactViewMinter: artifactMinter,
		SessionViewMinter:  sessionViewMinter,
		AgentUIMinter:      agentUIMinter,
	}
}

// setProgram backfills the *tea.Program after tea.NewProgram has
// returned. Called once, on the main goroutine, before program.Run().
func (s *chatSessionStarter) setProgram(p *tea.Program) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = p
}

// prog returns the backfilled *tea.Program under the mutex. It is
// always non-nil when submit/decideInteraction run, because setProgram is
// called before program.Run() starts the Update loop those callbacks fire
// from. The nil check is defensive: a nil return means "drop the
// event" rather than a nil-deref panic.
func (s *chatSessionStarter) prog() *tea.Program {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.program
}

// send routes a message to the TUI program. It is a no-op if the
// program is somehow not yet backfilled — see prog.
func (s *chatSessionStarter) send(msg tea.Msg) {
	if p := s.prog(); p != nil {
		p.Send(msg)
	}
}

// claimFirst atomically reports whether the caller is the FIRST submit
// (and, if so, marks the starter as started so no later call can also
// claim it). For a non-first call it also returns the current host so
// the caller can route without re-locking. The mutex serializes a fast
// double-submit: only one caller ever sees first==true.
func (s *chatSessionStarter) claimFirst() (first bool, host *local.Host) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.started = true
		return true, nil
	}
	return false, s.host
}

// submit is the model's input handler. On its first invocation it
// creates the AgentSession (with text as spec.prompt.inline) and builds
// the session-scoped infrastructure; on later invocations it routes the
// text through the host Listener. claimFirst serializes a fast
// double-submit so it cannot create two sessions or two hosts.
//
// The bubbletea model calls submit() SYNCHRONOUSLY from its Update
// loop, so all the slow work (session creation, the up-to-60s memory
// token wait, network round-trips) MUST run in a goroutine — otherwise
// the entire TUI freezes for the duration and the "starting the
// agent…" state can never render. Results/errors reach the TUI via
// program.Send, which is goroutine-safe.
func (s *chatSessionStarter) submit(text string) {
	go func() {
		first, host := s.claimFirst()
		if first {
			s.send(msgSessionStarting{})
			if err := s.startSession(text); err != nil {
				s.send(msgInboundResult{Err: err.Error()})
			}
			return
		}

		if host == nil {
			// First-message setup failed; the session never came up.
			s.send(msgInboundResult{Err: "session is not running"})
			return
		}
		dec, derr := host.Listener().SubmitUserMessage(context.Background(), text)
		if derr != nil {
			s.send(msgInboundResult{Err: derr.Error()})
			return
		}
		// OutcomeNoActiveSession means the session went terminal and the
		// pipeline (correctly) refused to spawn a phantom session for
		// this local channel — there is nothing to deliver to. Surface
		// it as a clean "the session has ended" TUI state, NOT a raw
		// error string: the TUI shows it and ends the interaction.
		s.send(msgInboundResult{
			Routed: dec.Outcome == channelkinds.OutcomeRouted,
			Notice: dec.Notice.ToWire(),
			Ended:  dec.Outcome == channelkinds.OutcomeNoActiveSession,
		})
	}()
}

// decideInteraction forwards a generic-interaction decision to the host
// Listener's SubmitInteractionDecision. Like submit it is called
// SYNCHRONOUSLY from the bubbletea Update loop, and the publish is a network
// round-trip, so the work runs in a goroutine to keep the Update loop
// unblocked. A decision arriving before the host exists is a visible no-op,
// not a nil dereference; publish errors reach the TUI via program.Send
// (goroutine-safe).
func (s *chatSessionStarter) decideInteraction(requestRef, category, actionID string) {
	go func() {
		s.mu.Lock()
		host := s.host
		s.mu.Unlock()
		if host == nil {
			s.send(local.MsgSendError{
				Kind: "interaction_decision",
				Err:  "session is not running yet", At: time.Now(),
			})
			return
		}
		if derr := host.Listener().SubmitInteractionDecision(
			context.Background(), s.b.Namespace, s.sessionName, requestRef, category, actionID); derr != nil {
			s.send(local.MsgSendError{
				Kind: "interaction_decision", Err: derr.Error(), At: time.Now(),
			})
		}
	}()
}

// startSession creates the AgentSession with the first message as the
// inline prompt, then builds the session-scoped infrastructure: a
// readiness wait → local host → outbound relay → phase watcher.
//
// The first message reaches the agent ONLY as spec.prompt.inline — the
// runner cold-starts with it as turn 0. It is deliberately NOT also
// routed through the inbound pipeline: the pipeline would append it as
// a second "inbox"-role turn, which the runner's drainInbox converts
// into a second "user" turn — so the agent would see (and act on) the
// task twice. Subsequent messages DO go through the pipeline (see
// submit's non-first branch); only the first is special.
//
// It returns any infrastructure failure (session create, started_by
// authz write, memory token wait, host/relay build) — the session
// never came up — so submit surfaces it as a TUI error and leaves host
// nil.
func (s *chatSessionStarter) startSession(prompt string) error {
	// The program is always backfilled by here: startSession only runs
	// from submit's goroutine, which the Update loop program.Run() drives
	// spawns — and setProgram is called before program.Run(). Grab it
	// once under the mutex for the programSink + phase watcher.
	program := s.prog()
	if program == nil {
		return fmt.Errorf("internal: TUI program not wired before startSession")
	}

	// Derive the ExternalIdentity the local listener will stamp on every
	// inbound event — used both as the started_by annotation on the session
	// and as the User on the host listener. Computing it once here keeps
	// the session annotation and the pipeline's identity consistent.
	chatUser := chatExternalIdentity(s.principal)

	// Create the channel-attached AgentSession with the first message as
	// spec.prompt.inline — the runner requires a non-empty prompt.
	sess := buildChatSession(s.b.Namespace, s.sessionName, s.agentClass, s.ch, prompt, chatUser.ExternalID.String(), chatUser.Email.String())
	if s.budgetTurns > 0 || s.budgetTokens > 0 || s.budgetDuration > 0 {
		sess.Spec.Budget = &spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    s.budgetTurns,
			MaxTokens:   s.budgetTokens,
			MaxDuration: metav1.Duration{Duration: s.budgetDuration},
		}
	}
	if err := s.b.Controller.Create(s.ctx, sess); err != nil {
		return fmt.Errorf("create AgentSession: %w", err)
	}
	s.mu.Lock()
	s.sess = sess
	s.mu.Unlock()

	// Establish the SpiceDB started_by relationship for the user.
	//
	// channelsd's pipeline writes started_by only on its own new-session
	// path (pipeline.Deliver). `oap agent chat` pre-creates the session
	// itself, so the pipeline always finds it as an *active* session and
	// goes straight to CheckInteract — which never writes started_by.
	// Without this explicit write, the FIRST follow-up message (routed
	// through the pipeline by submit) would fail CheckInteract
	// (interact = started_by + participant - denied, all empty) and be
	// denied. So we write it here, right after creating the session and
	// before any follow-up can arrive. The canonical ID must match what
	// the pipeline computes for the local user's inbound — same
	// identity.Principal inputs as pipeline.canonicalID.
	// Same inputs as pipeline.canonicalID, which opts into the synthetic
	// subject for a local user with no verified email — match it so the
	// started_by relation lines up with what the pipeline computes for this
	// user's inbound.
	startedByCanonical, err := s.principal.AllowSynthetic().Canonical()
	if err != nil {
		return fmt.Errorf("canonicalize started_by: %w", err)
	}
	if err := s.azClient.TouchStartedBy(s.ctx, s.b.Namespace, s.sessionName, startedByCanonical); err != nil {
		return fmt.Errorf("write started_by relationship: %w", err)
	}

	// Wait for the runner's per-session memory-token Secret purely as a
	// readiness signal. We deliberately do NOT read its Ed25519 audit-signing
	// seed: `oap` runs on a laptop and must not hold a credential that can
	// append to or forge a session's audit chain. channelsd performs the
	// inbound — it is the only component with a write-capable memory token
	// (pkg/memory/tokens/tokens.go:109-112).
	if err := waitForSessionReady(s.ctx, s.b, s.sessionName); err != nil {
		return err
	}

	// Build the artifact view-link minter best-effort. Either missing input
	// → minter stays nil; a single notice below explains why to the user.
	artifactMinter, minterReason := buildChatViewMinter(s.ctx, s.b)
	if minterReason != "" {
		// Print once to the TUI timeline so the user knows browser view links
		// are unavailable. This is informational — not a fatal error.
		s.send(msgLiveViewUnavailable{Reason: minterReason})
	}

	// Build the session-view link minter best-effort. A build failure reuses
	// the same startup-notice mechanism as the artifact minter above (the
	// TUI's one safe communication channel during an active bubbletea
	// program — there is no safe raw-stderr/stdout log sink here); the
	// session_view_offer sub-channel sender also surfaces a nil minter
	// loudly at the moment an escalation actually needs it, so this is
	// belt-and-suspenders, not the only signal.
	sessionViewMinter, sessionViewMinterReason := buildChatSessionViewMinter(s.ctx, s.b)
	if sessionViewMinterReason != "" {
		s.send(msgLiveViewUnavailable{Reason: "interactive session view: " + sessionViewMinterReason})
	}

	// Build the agent-UI link minter best-effort, the same way. The notice is
	// suppressed when it repeats the session-view one: both builders read the
	// same webd ConfigMap through readWebdTrustedBaseURL, so the common
	// failure — webd not installed — would otherwise print the identical cause
	// to the user twice, which reads as two separate faults. A reason that
	// DIFFERS is worth printing, because then the two reads genuinely
	// disagreed and the user is seeing something neither notice alone
	// explains.
	agentUIMinter, agentUIMinterReason := buildChatAgentUIMinter(s.ctx, s.b)
	if agentUIMinterReason != "" && agentUIMinterReason != sessionViewMinterReason {
		s.send(msgLiveViewUnavailable{Reason: "agent UI: " + agentUIMinterReason})
	}

	// tuiVia is the TUI surface's view URN. The inputs are the constant type
	// + empty id/sub, so Format cannot fail in practice — but per AGENTS.md
	// ("never silently drop errors") the error is checked rather than
	// discarded with `_`.
	tuiVia, err := viewurn.Format(viewurn.TypeTUI, "", "")
	if err != nil {
		return fmt.Errorf("mint tui view URN: %w", err)
	}

	host, err := local.NewHost(local.HostConfig{
		Deps:        s.hostDeps(artifactMinter, sessionViewMinter, agentUIMinter),
		Sink:        &programSink{program: program},
		User:        chatUser,
		Principal:   s.principal,
		Namespace:   s.b.Namespace,
		SessionName: s.sessionName,
		Via:         tuiVia,
	})
	if err != nil {
		return fmt.Errorf("build local host: %w", err)
	}

	// Outbound relay: session OUT subjects → the local host's senders.
	relay := newChatOutboundRelay(s.nc, s.b.Controller, host)
	if err := relay.Start(s.ctx); err != nil {
		return fmt.Errorf("start outbound relay: %w", err)
	}

	s.mu.Lock()
	s.host = host
	s.relay = relay
	s.mu.Unlock()

	// The relay is only subscribed NOW — after the AgentSession was created and
	// waited on — so anything published on the session's OUT subjects in that
	// window reached nobody. For a prompt the session parked on (a credential
	// link, an identity choice) that loss is permanent: the publisher dedups and
	// never re-sends, leaving the TUI showing a parked session with no card and
	// no reason. Ask channelsd to re-surface it now that we can receive it.
	// Best-effort — a failed nudge costs a card, not the session, so it is
	// surfaced in the TUI's startup-notice channel and the chat continues.
	if err := host.Listener().SubmitResurface(s.ctx, s.b.Namespace, s.sessionName); err != nil {
		s.send(msgStartupNotice{Text: "could not check for prompts the agent is waiting on: " + err.Error()})
	}

	// The first message is NOT routed through the host Listener: it
	// already reaches the agent as spec.prompt.inline (the runner
	// cold-starts with it as turn 0). Routing it would also append it as
	// an "inbox"-role turn, which the runner's drainInbox converts into a
	// second "user" turn — making the agent run the task twice. Only
	// subsequent messages go through the pipeline (see submit). The
	// msgSessionStarting already emitted by submit is the TUI's signal
	// that the first message was accepted; there is no inbound decision
	// to surface here.

	// Session-phase watcher: surface Failed/Succeeded into the TUI.
	go watchChatSessionPhase(s.ctx, s.b, s.sessionName, program)
	return nil
}

// newChatOutboundRelay builds the chat's outbound relay: session OUT subjects
// → the local host's senders, scoped to the one session this TUI is chatting
// with.
//
// The subscription is the cluster-wide "ap.session.*.*.out.>" for every
// consumer, and the CLI's NATS grant is "ap.>", so this relay is handed every
// Slack-, cron- and browser-driven session's outbound envelopes too. Accept
// rejects them from the Host's own (namespace, session) BEFORE the AgentSession
// is loaded, so foreign traffic costs no rate-limited API round-trip on the
// single goroutine this subscription's callbacks are serialized on — ahead of
// the chat user's own reply. The Host's resolver methods enforce the same scope
// again at delivery (see local.Host.owns); both halves are needed, and both
// read the same one pair of fields so they cannot disagree.
func newChatOutboundRelay(nc *nats.Conn, k8s client.Client, host *local.Host) *outbound.Relay {
	return &outbound.Relay{NC: nc, K8s: k8s, Senders: host, Accept: host.Accepts}
}

// teardown is the deferred cleanup. The AgentSession delete only runs
// if the session was actually created (the user may quit before typing
// anything); the relay is stopped only if it was started.
func (s *chatSessionStarter) teardown() {
	s.mu.Lock()
	relay := s.relay
	sess := s.sess
	s.mu.Unlock()

	if relay != nil {
		_ = relay.Stop(context.Background())
	}
	if sess != nil {
		// End the session on quit; memory + artifacts stay queryable.
		_ = s.b.Controller.Delete(context.Background(), sess)
	}
}

// programSink adapts a bubbletea *tea.Program to local.EventSink.
// tea.Program.Send is safe for concurrent use.
type programSink struct{ program *tea.Program }

func (s *programSink) Emit(msg any) { s.program.Send(msg) }

// chatExternalIdentity converts the resolved Principal to the
// channelkinds shape the local listener stamps on inbound events.
// A verified (logged-in) identity uses kind "idp" with the email as
// both Email and ExternalID so the canonical is the email basis;
// the legacy fallback keeps kind "local" + OS username.
//
// Invariant: identity.FromExternal(ext.Kind, ext.TeamScope,
// ext.ExternalID, ext.Email).Canonical() == p.Canonical() for both
// branches. The idp branch: FromExternal("idp","",email,email) gives
// Email=email → canonical = base64(email) = p.Canonical() ✓.
// The fallback branch: FromExternal("local","",user,"") gives
// canonical = base64("local::user") = p.Canonical() ✓.
func chatExternalIdentity(p identity.Principal) channelkinds.ExternalIdentity {
	if p.EmailVerified() {
		return channelkinds.ExternalIdentity{Kind: "idp", Email: p.Email(), ExternalID: identity.RawExternalID(p.Email())}
	}
	return channelkinds.ExternalIdentity{Kind: "local", ExternalID: p.ExternalID()}
}

// watchChatSessionPhase polls the AgentSession and sends a
// msgSessionPhase into the program when it reaches a terminal phase.
func watchChatSessionPhase(ctx context.Context, b *kube.Bundle, sessionName string, program *tea.Program) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var s spiceboxv1alpha1.AgentSession
			if err := b.Controller.Get(ctx, client.ObjectKey{
				Namespace: b.Namespace, Name: sessionName,
			}, &s); err != nil {
				continue // transient; retry next tick
			}
			switch s.Status.Phase {
			case spiceboxv1alpha1.AgentSessionPhaseFailed:
				reason, message := sessionFailureDetail(&s)
				program.Send(msgSessionPhase{
					Phase:          "Failed",
					FailureReason:  reason,
					FailureMessage: message,
				})
				return
			case spiceboxv1alpha1.AgentSessionPhaseSucceeded:
				program.Send(msgSessionPhase{Phase: "Succeeded"})
				return
			}
		}
	}
}

// sessionFailureDetail extracts the operator-facing failure reason and
// message from a Failed AgentSession. It prefers the Failed condition's
// Reason+Message (which carries the human-readable "what" — e.g.
// "runner restarted 5 times") and falls back to Status.FailureReason
// when no condition is present.
func sessionFailureDetail(s *spiceboxv1alpha1.AgentSession) (reason, message string) {
	reason = s.Status.FailureReason
	if c := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed); c != nil {
		if c.Reason != "" {
			reason = c.Reason
		}
		message = c.Message
	}
	return reason, message
}

// dialNATSForChat connects to the port-forwarded NATS with indefinite
// reconnect so the TUI's relay survives a port-forward blip. opts
// carries the CLI's creds + CA + ServerName (from clinats.LoadCreds);
// the URL is the port-forwarded local address.
func dialNATSForChat(opts apnats.Options, localPort uint16) (*nats.Conn, error) {
	opts.URL = fmt.Sprintf("nats://127.0.0.1:%d", localPort)
	nc, err := apnats.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", opts.URL, err)
	}
	return nc, nil
}

// waitForSessionReady blocks until the chat session is ready for the TUI to
// render — meaning EITHER the session has STARTED
// (spiceboxv1alpha1.AgentSessionPhaseStarted) or the operator has minted the
// per-session `<name>-memory-token` Secret.
//
// The phase branch is what matters for a session that parks awaiting the user
// (AwaitingCredentials / AwaitingIdentityChoice): it has no runner and thus
// never gets a memory-token Secret, yet the TUI MUST proceed so the credential /
// identity prompt renders through the render stream. Blocking (or erroring) here
// instead returns from startup, which fires the deferred teardown and DELETES
// the parked session — the "prompt never shows, then it's gone" bug. It is
// checked first, so a parked session is ready even under a cancelled context.
//
// The token branch is the precise "runner up, channelsd can inbound" signal for
// a normal session. Its presence is the readiness signal; its contents are NOT
// read — the Secret also carries an Ed25519 audit-signing seed, the credential
// that would let `oap` (which runs on a laptop) forge this session's audit chain,
// so this function only ever checks for a non-empty bearer token and never
// touches the seed. channelsd is the sole component that reads it
// (pkg/memory/tokens/tokens.go:109-112).
func waitForSessionReady(ctx context.Context, b *kube.Bundle, sessionName string) error {
	err := wait.Until(ctx, 1*time.Second, 60*time.Second, func(ctx context.Context) (bool, error) {
		var sess spiceboxv1alpha1.AgentSession
		if err := b.Controller.Get(ctx, client.ObjectKey{
			Namespace: b.Namespace, Name: sessionName,
		}, &sess); err == nil && spiceboxv1alpha1.AgentSessionPhaseStarted(sess.Status.Phase) {
			return true, nil
		}
		var sec corev1.Secret
		if err := b.Controller.Get(ctx, client.ObjectKey{
			Namespace: b.Namespace, Name: sessionName + "-memory-token",
		}, &sec); err != nil {
			return false, nil // may not exist yet
		}
		return len(sec.Data["token"]) > 0, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for session %q to become ready: %w", sessionName, err)
	}
	return nil
}
