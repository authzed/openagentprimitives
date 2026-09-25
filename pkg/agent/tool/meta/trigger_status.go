package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/triggerstatus"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// TriggerStatusConfig wires the two trigger-status tools to the session's INPUT
// channel binding.
//
// Note what is NOT here: no repository, no issue number, no commit, no
// provider handle of any kind. Everything that identifies the trigger travels
// on Binding, which the channel kind itself wrote when the event arrived, and
// the kind reads back out. That is the whole point — the identifiers were never
// the agent's to carry, and a config that accepted them would put them back
// within reach of a mistake.
type TriggerStatusConfig struct {
	// KindName is the input binding's denormalized channel kind, resolved
	// through the channel-kind registry at CALL time. A name rather than a
	// resolved Kind so tool assembly need not hold one, and so the two tools
	// and the capability all reach their reporter by the one shared lookup.
	KindName string

	// SurfaceKind is the kind's own generic name for what it reports on ("a
	// pull request's GitHub check run"), from TriggerStatusReporter's
	// TriggerSurfaceKind. It reaches the model in these tools' descriptions, so
	// the wording belongs to the kind and is passed through verbatim.
	SurfaceKind string

	// ChannelName is the input Channel CR, in the session's namespace. Resolved
	// at call time along with its credentials Secret: a session outlives a
	// credential rotation, and a Secret captured at assembly would be the stale
	// one at the moment the review concludes.
	ChannelName string

	// Binding is the session's input ChannelBinding — the record of WHICH
	// event started this session.
	Binding *spiceboxv1alpha1.ChannelBinding

	// ProviderAPIBaseURL overrides the provider's API host. Empty in
	// production; the only seam that lets a test point these calls at a
	// stand-in server, mirroring the channel controller's own GitHubAPIBaseURL.
	ProviderAPIBaseURL string

	// WebdBaseURL returns webd's externally reachable base URL, and is what
	// lets conclude_trigger_status fill in its own details link rather than
	// asking a model to assemble one.
	//
	// A getter rather than a string because the value can land after this
	// session started — `oap install` seeds the ConfigMap empty while it
	// manages external access — and "" means "not addressable yet", which the
	// caller treats as a clean skip, never as a failure to conclude. Nil in a
	// process that has no webd at all.
	WebdBaseURL func() string

	// ComposedTextOnly, when true, removes every model-authored byte from
	// what this session publishes on its trigger surface. The conclude tool
	// takes only the outcome enum; the published summary is a fixed text per
	// outcome (composedSummary), and the details link is always the
	// framework's own deliveredResultLink — a model-supplied summary or URL
	// is structurally ignored, never published. A class whose surface is
	// readable by more people than its channel (a GitHub check run on a
	// repository) opts in via the trigger_status capability's
	// {publishedText: composed} config, so a security finding delivered
	// through the channel cannot be re-described in public by the status
	// that announces it. False (the zero value) is today's behavior.
	ComposedTextOnly bool
}

// NewClaimTriggerStatus builds claim_trigger_status: mark the event that
// started this session as being worked on, and learn whether it already carries
// an answer.
func NewClaimTriggerStatus(cfg TriggerStatusConfig) tool.Tool {
	return &claimTriggerStatusTool{cfg: cfg}
}

// NewConcludeTriggerStatus builds conclude_trigger_status: write the final
// answer back to the event that started this session.
func NewConcludeTriggerStatus(cfg TriggerStatusConfig) tool.Tool {
	return &concludeTriggerStatusTool{cfg: cfg}
}

// ---------------------------------------------------------------------
// claim_trigger_status
// ---------------------------------------------------------------------

type claimTriggerStatusTool struct{ cfg TriggerStatusConfig }

func (*claimTriggerStatusTool) Name() string    { return "claim_trigger_status" }
func (*claimTriggerStatusTool) Kind() tool.Kind { return tool.KindMeta }

// Permission: the write goes to a surface the operator's own app already owns,
// scoped to the event that started this session and to nothing else — the tool
// cannot be pointed anywhere, because it takes no target. Session-scoped and
// gated by AgentSession#interact like the other channel-facing meta tools.
func (*claimTriggerStatusTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*claimTriggerStatusTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *claimTriggerStatusTool) Description() string {
	return "Mark the event that started this session as being worked on now, and find out whether it " +
		"has already been answered. This session was started by " + t.cfg.SurfaceKind + ", and this " +
		"marks it in progress so whoever is watching can see the work has begun. Takes no arguments: " +
		"which event, and where its status lives, come from the event itself. " +
		"If the result says `already_concluded`, this exact state of the work was answered before — a " +
		"repeat delivery — so there is nothing to redo; say nothing further and end the round."
}

// InputSchema is deliberately empty. Every identifier this call needs came in
// on the event, so there is nothing for a model to supply and nothing for it to
// get wrong.
func (*claimTriggerStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)
}

func (t *claimTriggerStatusTool) Execute(ctx context.Context, _ json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	surface, res, ok := t.cfg.resolve(ctx, t.Name(), sess)
	if !ok {
		return res, nil
	}
	claim, err := surface.Claim(ctx)
	if err != nil {
		return failed(t.Name(), fmt.Sprintf("could not claim %s: %v", surface.Surface(), err)), nil
	}
	if claim.Concluded {
		// The redelivery path, and the second of the two ways a trigger comes
		// to carry an answer. This result tells the model to say nothing
		// further and end the round; without recording it, the completion gate
		// would then refuse to let it, over an answer the surface already
		// holds.
		recordConcluded(ctx, sess, claim.Outcome, t.Name())
	}

	out, err := json.Marshal(struct {
		Surface          string `json:"surface"`
		Ref              string `json:"ref,omitempty"`
		AlreadyConcluded bool   `json:"already_concluded"`
		Outcome          string `json:"outcome,omitempty"`
	}{
		Surface:          surface.Surface(),
		Ref:              claim.Ref,
		AlreadyConcluded: claim.Concluded,
		Outcome:          string(claim.Outcome),
	})
	if err != nil {
		return failed(t.Name(), fmt.Sprintf("claimed %s but could not render the result: %v", surface.Surface(), err)), nil
	}
	return tool.Result{Content: string(out), Trusted: true}, nil
}

// ---------------------------------------------------------------------
// conclude_trigger_status
// ---------------------------------------------------------------------

type concludeTriggerStatusTool struct{ cfg TriggerStatusConfig }

func (*concludeTriggerStatusTool) Name() string    { return "conclude_trigger_status" }
func (*concludeTriggerStatusTool) Kind() tool.Kind { return tool.KindMeta }

func (*concludeTriggerStatusTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*concludeTriggerStatusTool) PermissionVariants() []authz.PermissionVariant { return nil }

// Description branches whole rather than sharing a prefix: the model-text
// mode's text is a stable contract composed mode must not perturb, and a
// shared fragment would let an edit meant for one mode silently rewrite the
// other's prompt.
func (t *concludeTriggerStatusTool) Description() string {
	if t.cfg.ComposedTextOnly {
		return "Record how this round of work turned out, on the event that started this session — " +
			t.cfg.SurfaceKind + ". Whoever is watching that event sees this as the answer, so call it once " +
			"you have delivered your result and not before: what is recorded here is what a person reads " +
			"instead of waiting. You supply only the outcome; the published summary and details link are composed " +
			"for you, and nothing you write reaches the surface. Which event, and where its status " +
			"lives, come from the event itself."
	}
	return "Record how this round of work turned out, on the event that started this session — " +
		t.cfg.SurfaceKind + ". Whoever is watching that event sees this as the answer, so call it once " +
		"you have delivered your result and not before: what you write here is what a person reads " +
		"instead of waiting. You supply only the judgement — the outcome and a short summary. Which " +
		"event, and where its status lives, come from the event itself."
}

// InputSchema offers the outcome enum DERIVED from the seam's own closed set,
// never retyped. The schema is what a model chooses from and ParseTriggerOutcome
// is what accepts the answer; a value in one and not the other would be a tool
// proposing an argument it then refuses. In composed mode the outcome is the
// ONLY property: the schema is the prompt, and a summary field left in it
// would invite the exact bytes composed mode exists to keep off the surface.
func (t *concludeTriggerStatusTool) InputSchema() json.RawMessage {
	enum, err := json.Marshal(channelkinds.TriggerOutcomes())
	if err != nil {
		// Marshalling a slice of strings cannot fail; a schema with no enum
		// would silently widen what the model may propose, so this refuses to
		// produce one rather than degrade quietly.
		panic("meta: rendering the trigger-outcome enum: " + err.Error())
	}
	outcomeProp := `"outcome": {
			"type": "string",
			"enum": ` + string(enum) + `,
			"description": "How the work turned out. 'clean': you did the work and found nothing that should block. 'problems_found': you did the work and it should not proceed as-is — a verdict on the work, not a failure of yours. 'could_not_finish': you could not do the work at all (a refusal, a failed checkout, an exhausted budget)."
		}`
	if t.cfg.ComposedTextOnly {
		return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			` + outcomeProp + `
		},
		"required": ["outcome"]
	}`)
	}
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			` + outcomeProp + `,
			"summary": {
				"type": "string",
				"description": "A short summary a person reads on the event itself. Required. Write it for someone who will not open the full result: say what you found, or why you could not finish."
			},
			"details_url": {
				"type": "string",
				"description": "Optional. Leave this out and a link to the result you delivered is filled in for you. Supply an absolute http(s) link ONLY when the full result lives somewhere else entirely — a build log, an external dashboard. A relative or non-http link is refused."
			}
		},
		"required": ["outcome", "summary"]
	}`)
}

func (t *concludeTriggerStatusTool) Execute(ctx context.Context, args json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var in struct {
		Outcome    string `json:"outcome"`
		Summary    string `json:"summary"`
		DetailsURL string `json:"details_url,omitempty"`
	}
	example := `{"outcome": "problems_found", "summary": "Two findings in the auth path.", "details_url": "https://example.test/thread/1"}`
	if t.cfg.ComposedTextOnly {
		example = `{"outcome": "problems_found"}`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), example); !ok {
		return res, nil
	}
	// Validated against the seam's own parser, before anything is resolved: the
	// outcome is the one judgement the framework cannot make, and a call that
	// cannot state it must not reach a provider at all.
	outcome, err := channelkinds.ParseTriggerOutcome(in.Outcome)
	if err != nil {
		return failed(t.Name(), err.Error()), nil
	}
	if !t.cfg.ComposedTextOnly && strings.TrimSpace(in.Summary) == "" {
		return failed(t.Name(), "`summary` is required and must be non-empty — it is what a person reads instead of waiting for you"), nil
	}

	surface, res, ok := t.cfg.resolve(ctx, t.Name(), sess)
	if !ok {
		return res, nil
	}
	// In composed mode nothing the model wrote is published: the summary is
	// composed from the outcome alone, and the details link is always the
	// framework's own. A model-supplied summary or details_url — a chatty
	// model, or a prompt-injected one — is structurally ignored here, which
	// is the deterministic half of the guarantee; the schema not asking is
	// only the polite half.
	summary := in.Summary
	// The agent's own link wins when it supplied one; otherwise the framework
	// names the result it actually delivered. Trimmed first, because a model
	// that emits whitespace meant to supply nothing, and a whitespace details
	// URL is one the kind refuses — failing a conclusion over punctuation.
	detailsURL := strings.TrimSpace(in.DetailsURL)
	if t.cfg.ComposedTextOnly {
		var err error
		summary, err = composedSummary(outcome)
		if err != nil {
			return failed(t.Name(), err.Error()), nil
		}
		detailsURL = ""
	}
	if detailsURL == "" {
		detailsURL = t.cfg.deliveredResultLink(ctx, sess)
	}
	if err := surface.Conclude(ctx, channelkinds.TriggerConclusion{
		Outcome:    outcome,
		Summary:    summary,
		DetailsURL: detailsURL,
	}); err != nil {
		return failed(t.Name(), fmt.Sprintf("could not conclude %s: %v", surface.Surface(), err)), nil
	}
	// Recorded only now that the answer is actually on the surface. A record
	// written ahead of the call would tell the completion gate a pull request
	// was answered when the publish had failed.
	recordConcluded(ctx, sess, outcome, t.Name())

	// The PARSED outcome, not the raw argument: what the result reports is what
	// was actually published, and echoing the input would let the two differ if
	// the parser ever canonicalized.
	content := "recorded on " + surface.Surface() + ": " + string(outcome)
	if t.cfg.ComposedTextOnly {
		content += " (a composed summary was published; model-supplied text does not reach this surface)"
	}
	return tool.Result{Content: content, Trusted: true}, nil
}

// composedSummary is the complete set of texts conclude_trigger_status can
// publish for a class that opted into composed-only publication. Fixed per
// outcome and authored HERE, so that with the schema's title (derived from
// the outcome by the kind) every byte on the surface is operator- or
// framework-authored: the surface may be readable by far more people than
// the session's channel, and the findings themselves must only ever travel
// through the channel. The default arm cannot be reached through
// ParseTriggerOutcome's closed set; it exists so a future outcome added to
// the seam fails a conclusion loudly here rather than publishing silence.
func composedSummary(o channelkinds.TriggerOutcome) (string, error) {
	switch o {
	case channelkinds.TriggerOutcomeClean:
		return "This round concluded clean: the work finished and found nothing blocking. The full result was delivered through this session's channel.", nil
	case channelkinds.TriggerOutcomeProblemsFound:
		return "This round concluded with findings that need attention. The findings were delivered through this session's channel and are deliberately not repeated here.", nil
	case channelkinds.TriggerOutcomeCouldNotFinish:
		return "This round could not be finished. The reason was delivered through this session's channel.", nil
	default:
		return "", fmt.Errorf("no composed summary is defined for outcome %q; refusing to publish", o)
	}
}

// deliveredResultLink returns the durable artifact-view link for the result
// this session delivered, or "" when there is nothing to link to.
//
// Why the framework composes this rather than the model: every input is
// already known here and none of them is the agent's. The session names
// itself, the deliveries store records which artifact respond_to_user actually
// put in front of a person, and webd's base URL comes from the cluster. A
// model asked for the same link can only reconstruct it from what it happens
// to remember, which is how a check run ends up pointing at a thread nobody
// outside the workspace can open.
//
// The link is deliberately the SUBJECT-INDEPENDENT one
// (channelkinds.ComposeArtifactViewURL), never a minted view link: it lands on
// a pull request read by everyone who can read the repository, and a link
// minted for the agent's own subject would hand every one of them the agent's
// view. This one carries no authority — webd authorizes each visitor on
// arrival.
//
// Every "nothing to link to" path returns "" so the conclusion still
// publishes: an unanswered trigger is the failure this seam exists to end, and
// withholding the answer over a missing link would be strictly worse than
// publishing it without one. Each is logged, because a check run that quietly
// stopped carrying a link is otherwise untraceable to here.
func (cfg TriggerStatusConfig) deliveredResultLink(ctx context.Context, sess *tool.SessionContext) string {
	logger := log.FromContext(ctx)
	if cfg.WebdBaseURL == nil {
		logger.Info("conclude_trigger_status: no webd base URL is wired into this runner, so the trigger will carry no details link",
			"session", sess.Namespace+"/"+sess.Name)
		return ""
	}
	delivered, ok := deliveries.TryFrom(sess)
	if !ok {
		logger.Info("conclude_trigger_status: this session carries no delivery state, so the trigger will carry no details link",
			"session", sess.Namespace+"/"+sess.Name)
		return ""
	}
	artifactID := delivered.LastArtifactID()
	if artifactID == "" {
		// Ordinary for a session that answered in prose alone; also what a
		// render carrying no artifact-id label produces.
		logger.Info("conclude_trigger_status: this session delivered no linkable artifact, so the trigger will carry no details link",
			"session", sess.Namespace+"/"+sess.Name)
		return ""
	}
	sessionRef := sess.Namespace + "/" + sess.Name
	url, err := channelkinds.ComposeArtifactViewURL(cfg.WebdBaseURL(), sessionRef, artifactID)
	if err != nil {
		logger.Info("conclude_trigger_status: could not compose the artifact link; the trigger will carry no details link",
			"session", sessionRef, "artifactID", artifactID, "err", err.Error())
		return ""
	}
	if url == "" {
		logger.Info("conclude_trigger_status: webd has no external URL yet, so the trigger will carry no details link",
			"session", sessionRef, "artifactID", artifactID)
	}
	return url
}

// recordConcluded leaves the fact that this session's trigger now carries an
// answer where the `trigger-status-concluded` completion requirement reads it.
//
// Best-effort by design, and never silent. The answer IS published by the time
// this runs, so failing the tool call over the bookkeeping would be worse than
// the consequence of losing it — which is an over-strict completion gate later,
// bypassable with a stated reason. The log line names that consequence, because
// the visible symptom (agent_work_complete refusing over a check run the pull
// request already shows as concluded) is otherwise untraceable to here.
func recordConcluded(ctx context.Context, sess *tool.SessionContext, outcome channelkinds.TriggerOutcome, toolName string) {
	logger := log.FromContext(ctx)
	store, ok := triggerstatus.TryFrom(sess)
	if !ok {
		logger.Info(toolName+": this session carries no trigger-status state, so the completion gate cannot see that the trigger was answered",
			"session", sess.Namespace+"/"+sess.Name, "outcome", string(outcome))
		return
	}
	if err := store.RecordConcluded(ctx, outcome); err != nil {
		logger.Info(toolName+": failed to record the trigger conclusion; the completion gate may report this trigger as unanswered",
			"session", sess.Namespace+"/"+sess.Name, "outcome", string(outcome), "err", err.Error())
	}
}

// ---------------------------------------------------------------------
// shared resolution
// ---------------------------------------------------------------------

// resolve turns the config into a live TriggerSurface, or into the tool result
// explaining why it could not.
//
// Both tools go through here so they cannot disagree about how a surface is
// reached, and every step reports: a session whose trigger status cannot be
// written is one where somebody outside is waiting on an answer that will never
// come, which is exactly the silence this whole seam exists to end.
//
// The (result, ok) shape rather than an error because every failure here is a
// tool_result the model can read and act on — retry, or say plainly that it
// could not record the outcome — not a runner-level fault.
func (cfg TriggerStatusConfig) resolve(ctx context.Context, toolName string, sess *tool.SessionContext) (channelkinds.TriggerSurface, tool.Result, bool) {
	if sess == nil {
		return nil, failed(toolName, "no session context"), false
	}
	if cfg.Binding == nil {
		return nil, failed(toolName, "this session records no input channel binding, so it has no trigger to report on"), false
	}
	c, ok := sess.K8sClient.(client.Client)
	if !ok || c == nil {
		return nil, failed(toolName, "no Kubernetes client on this session, so its Channel credentials cannot be read"), false
	}
	// THE shared lookup — the same one tool assembly used to decide these tools
	// exist at all. Resolved here rather than captured at assembly so both
	// consumers reach a kind by exactly one route.
	reporter, ok := chregistry.TriggerStatusReporterFor(cfg.KindName)
	if !ok {
		return nil, failed(toolName, fmt.Sprintf("channel kind %q reports no trigger status in this build", cfg.KindName)), false
	}

	var ch spiceboxv1alpha1.Channel
	if err := c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: cfg.ChannelName}, &ch); err != nil {
		return nil, failed(toolName, fmt.Sprintf("could not read Channel %s: %v", cfg.ChannelName, err)), false
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ch.Spec.CredentialsRef.SecretName}, &sec); err != nil {
		return nil, failed(toolName, fmt.Sprintf("could not read the credentials Secret for Channel %s: %v", cfg.ChannelName, err)), false
	}

	surface, err := reporter.TriggerSurface(&ch, cfg.Binding, channelkinds.WebhookSecrets{Data: sec.Data},
		channelkinds.TriggerStatusOptions{ProviderAPIBaseURL: cfg.ProviderAPIBaseURL})
	if err != nil {
		return nil, failed(toolName, err.Error()), false
	}
	if surface == nil {
		// The kind's "this binding carries no trigger" answer. Reported rather
		// than swallowed: the tools were offered, so something upstream
		// believed there was a surface here and the disagreement is worth
		// seeing.
		return nil, failed(toolName, fmt.Sprintf("channel kind %q reports no trigger status for this session's binding", cfg.KindName)), false
	}
	return surface, tool.Result{}, true
}

// failed renders one of these tools' refusals.
//
// Trusted, and the claim needs stating rather than assuming, because Trusted's
// zero value is the safe one and every one of these tools' results sets it.
// Every word here is written by this file or by the channel kind. The only
// third-party-derived text that reaches a result at all is the identifier the
// kind names its surface with — a repository path and a number, parsed out of a
// binding key this same kind wrote from a signature-verified payload, over a
// charset the provider constrains. A provider RESPONSE BODY never reaches one:
// checkruns.Client's errors carry a path and a status code and deliberately
// nothing else, which is what keeps that true.
//
// It matters most on this path: a refusal is what the model reads to correct
// its own call, and a guard that redacted it would leave a round unable to
// answer its trigger for reasons the model could not see.
func failed(toolName, msg string) tool.Result {
	return tool.Result{Content: toolName + ": " + msg, IsError: true, Trusted: true}
}
