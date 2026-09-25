package slack

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// RefreshScopes implements channelkinds.ScopeRefresher: channelsd calls it on
// its existing reconcile tick, handing over the Channel as freshly listed, so
// an edit to the bound agent's capabilities — or a rebind to a different agent
// — is reflected in ScopesValid without a restart.
func (l *slackListener) RefreshScopes(ctx context.Context, ch *spiceboxv1alpha1.Channel) {
	l.refreshScopesValid(ctx, ch)
}

var _ channelkinds.ScopeRefresher = (*slackListener)(nil)

// agentClassRefreshInterval bounds how often one listener re-reads its bound
// AgentClass. The reconcile tick runs every 5s; reading the class on every one
// of them would cost one GET per slack Channel per 5s — N/5 QPS against
// channelsd's client, which takes client-go's DefaultQPS of 5 because
// rest.InClusterConfig leaves QPS zero. At 25 Channels that alone saturates the
// limiter, and inbound message handling (AgentSession creates, Secret reads)
// queues behind it. Throttling to once a minute makes the steady-state cost
// N/60 QPS while keeping the condition fresh within a minute of a capability
// edit.
//
// A rebind is NOT subject to this: scopeFeaturesFor invalidates the cache the
// moment spec.agentClass names a different class, so re-pointing a Channel at
// another agent is reflected on the next tick.
//
// The comparison that decides whether to WRITE is never throttled or cached —
// it runs every tick against the Channel the caller just listed.
const agentClassRefreshInterval = time.Minute

// scopeHeaderRecheckMissing and scopeHeaderRecheckGranted bound how often one
// listener re-reads its GRANTED scopes from Slack, by asking auth.test again.
//
// That re-read is the only thing that can make the ScopesValid=False message's
// own remedy work. Adding a scope and re-installing the Slack app grants it on
// the SAME bot token, so no Secret, Channel or AgentClass in the cluster
// changes — nothing else in the system has any reason to look again, and the
// condition would keep reporting the connect-time answer until channelsd
// restarts.
//
// The reconcile tick runs every 5s per Channel, so an unthrottled re-read
// would be 12 auth.test calls per minute per Channel, forever, against a
// per-app rate limit this cluster does not control, to catch an event that
// happens a handful of times in a Channel's life. Two intervals instead,
// chosen from the answer the tick just computed:
//
//   - Scopes MISSING (scopeHeaderRecheckMissing): a minute. An operator has
//     just been handed an instruction and is acting on it, so the condition
//     has to clear while they are still watching — a longer wait is what sends
//     them to restart the deployment instead. It matches
//     agentClassRefreshInterval, so the condition converges within a minute of
//     a change on EITHER side, the agent's or Slack's.
//   - Scopes GRANTED (scopeHeaderRecheckGranted): an hour. Nobody is waiting
//     on it; this only catches a scope being REMOVED by a later re-install,
//     which is otherwise invisible until a restart. One call per hour per
//     Channel is noise next to the traffic a live Channel already makes.
const (
	scopeHeaderRecheckMissing = time.Minute
	scopeHeaderRecheckGranted = time.Hour
)

// scopeHeaderRecheckTimeout bounds a single granted-scope re-read. The call
// runs off the reconcile goroutine, so a hang costs nothing but the goroutine
// — but an unbounded one would hold the in-flight flag forever and the
// condition would never converge again.
const scopeHeaderRecheckTimeout = 15 * time.Second

// missingScopes returns the elements of required that are not present in the
// comma-separated value of the X-OAuth-Scopes response header. Slack sets that
// header on every API response, so it is the canonical view of what the
// installed app actually has, with no extra call.
//
// required is supplied by the caller, not read from any static package-level
// list — see features.go for the per-feature scope map that
// channelkinds.ScopesFor draws from, and requiredScopes below for the set the
// BOUND AGENT actually needs.
//
// An empty header with a non-empty required set is treated as "everything
// missing": Slack normally always populates it, so an empty one is suspicious
// enough that operators should see it rather than have it silently pass. The
// distinct case of "auth.test has not run yet" never reaches here — see
// refreshScopesValid.
func missingScopes(grantedHeader string, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	if strings.TrimSpace(grantedHeader) == "" {
		out := append([]string(nil), required...)
		sort.Strings(out)
		return out
	}
	granted := make(map[string]struct{}, 16)
	for _, s := range strings.Split(grantedHeader, ",") {
		if s = strings.TrimSpace(s); s != "" {
			granted[s] = struct{}{}
		}
	}
	var missing []string
	for _, want := range required {
		if _, ok := granted[want]; !ok {
			missing = append(missing, want)
		}
	}
	sort.Strings(missing)
	return missing
}

// recordGrantedScopes caches the X-OAuth-Scopes header from a SUCCESSFUL
// auth.test. Calling it is what makes an empty header meaningful: after this,
// "" means Slack said the token grants nothing; before it, "" means nobody has
// asked.
//
// It also stamps when the header was obtained, which is what keeps Start's own
// auth.test from being repeated by the tick moments later: the re-read is due
// only once the cached answer is actually stale.
func (l *slackListener) recordGrantedScopes(header string) {
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	l.grantedScopeHeader = header
	l.grantedScopesKnown = true
	l.grantedScopesCheckedAt = l.nowOrWall()
}

// scheduleGrantedScopeRecheck starts an out-of-band re-read of the granted
// scopes when the cached header has gone stale, at the rate this tick's answer
// calls for (scopeHeaderRecheckMissing when scopes are missing, the slower
// scopeHeaderRecheckGranted when they are not).
//
// OUT OF BAND deliberately. channelsd calls RefreshScopes synchronously, from
// inside the per-Channel loop of its single reconcile goroutine, so a Slack
// round-trip taken inline would put every OTHER Channel's reconcile — listener
// start and stop, Connected, the bento stream — behind one slow call, turning
// a status-only nicety into an availability problem the moment Slack is slow.
//
// The goroutine's only job is to refresh the cache. The condition itself is
// recomputed by the next tick, at most 5s later, against that tick's freshly
// listed Channel — which is also what keeps the re-read from having to know
// about a rebind it would otherwise answer with a stale AgentClass.
func (l *slackListener) scheduleGrantedScopeRecheck(ctx context.Context, scopesMissing bool) {
	if l.api == nil {
		// No Slack transport wired. In production Start fails outright when
		// the tokens are missing, so this is a listener constructed standalone
		// (tests); there is nobody to ask, and nothing has gone wrong.
		log.FromContext(ctx).V(1).Info("slack listener: no Slack client; not re-reading the granted scopes",
			"channel", listenerChannelName(l))
		return
	}
	interval := scopeHeaderRecheckGranted
	if scopesMissing {
		interval = scopeHeaderRecheckMissing
	}
	if !l.claimGrantedScopeRecheck(interval) {
		return
	}
	go l.recheckGrantedScopes(ctx)
}

// claimGrantedScopeRecheck reports whether this caller should perform the
// re-read, and marks it as taken when so.
//
// The check is stamped on the ATTEMPT rather than on success: a Slack that is
// down — or rate-limiting, the most likely reason a re-read fails at all —
// must be backed off from, not asked again on each of the next 5s ticks.
func (l *slackListener) claimGrantedScopeRecheck(interval time.Duration) bool {
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	if l.grantedScopesRechecking {
		return false
	}
	now := l.nowOrWall()
	if !l.grantedScopesCheckedAt.IsZero() && now.Sub(l.grantedScopesCheckedAt) < interval {
		return false
	}
	l.grantedScopesCheckedAt = now
	l.grantedScopesRechecking = true
	return true
}

// finishGrantedScopeRecheck releases the claim taken by claimGrantedScopeRecheck.
func (l *slackListener) finishGrantedScopeRecheck() {
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	l.grantedScopesRechecking = false
}

// recheckGrantedScopes asks Slack what this bot token is granted NOW and
// caches the answer. Re-installing the app with an added scope grants it on
// the same token, so this call is the only event in the system that can
// observe it — nothing in the cluster changes, and the connect-time header
// would otherwise stand until channelsd restarts.
//
// A FAILED re-read leaves the last known header standing rather than clearing
// it. missingScopes reads an empty header as "everything missing", so clearing
// would turn one rate-limited call into a full-outage condition on a Channel
// whose token is fine — the same flap activeFeatures already refuses to make
// when the AgentClass read fails.
func (l *slackListener) recheckGrantedScopes(ctx context.Context) {
	defer l.finishGrantedScopeRecheck()
	logger := log.FromContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, scopeHeaderRecheckTimeout)
	defer cancel()

	auth, err := l.api.AuthTestContext(ctx)
	if err != nil {
		logger.Info("slack listener: re-reading the granted bot-token scopes failed; keeping the last known set",
			"channel", listenerChannelName(l), "err", err.Error())
		return
	}
	if auth == nil {
		// Defensive: this runs on its own goroutine, where a nil dereference
		// would take channelsd down rather than fail one Channel's status.
		logger.Info("slack listener: auth.test returned no response while re-reading the granted bot-token scopes; keeping the last known set",
			"channel", listenerChannelName(l))
		return
	}
	l.recordGrantedScopes(auth.Header.Get("X-OAuth-Scopes"))
}

// grantedScopes returns the cached header and whether auth.test has answered.
func (l *slackListener) grantedScopes() (header string, known bool) {
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	return l.grantedScopeHeader, l.grantedScopesKnown
}

// now returns the listener's clock (wall time unless a test injected one).
func (l *slackListener) nowOrWall() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// cachedFeatures returns the cached feature set when it was resolved for
// agentClass and is younger than agentClassRefreshInterval.
func (l *slackListener) cachedFeatures(agentClass string) ([]channelfeatures.Feature, bool) {
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	if l.scopeFeatures == nil || l.scopeFeaturesFor != agentClass {
		return nil, false
	}
	if l.nowOrWall().Sub(l.scopeFeaturesAt) >= agentClassRefreshInterval {
		return nil, false
	}
	return l.scopeFeatures, true
}

func (l *slackListener) cacheFeatures(agentClass string, features []channelfeatures.Feature) {
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	l.scopeFeatures = features
	l.scopeFeaturesFor = agentClass
	l.scopeFeaturesAt = l.nowOrWall()
}

// activeFeatures resolves the channel features the bound agent actually uses,
// from the capabilities on the AgentClass ch binds — ch being the Channel as of
// this tick, so a rebind is honored rather than the start-time snapshot.
//
// Falls back to the kind-agnostic baseline when there is no class to read: a
// Channel that binds none (spec.agentClass empty), or one naming a class that
// does not exist yet or was deleted. Failing SAFE there is deliberate —
// reporting every optional scope as missing because a class was momentarily
// unreadable would train operators to ignore the condition, which is the exact
// failure this seam exists to prevent. Each case is logged, distinctly.
//
// A READ FAILURE is treated differently from an absent class: the last resolved
// answer is kept when there is one, because a transient API error must not flip
// a genuine "missing files:write" to a clean bill of health for a minute.
func (l *slackListener) activeFeatures(ctx context.Context, ch *spiceboxv1alpha1.Channel) []channelfeatures.Feature {
	logger := log.FromContext(ctx)
	if ch == nil {
		return channelfeatures.Baseline()
	}
	name := ch.Spec.AgentClass
	if cached, ok := l.cachedFeatures(name); ok {
		return cached
	}

	// Declared as the concrete pointer ActiveFor takes, assigned only on a
	// successful read: nil here means "no class", which ActiveFor answers with
	// the baseline.
	var class *spiceboxv1alpha1.AgentClass
	if name == "" {
		// No class bound: nothing declares cross-agent participation, so it is
		// off. Stated rather than left at the zero value, because a Channel
		// REBOUND from a cross-agent class to none must lose the admission —
		// the listener is not restarted for a spec edit.
		l.setCrossAgent(nil)
	}
	if name != "" && l.deps.K8sClient != nil {
		var ac spiceboxv1alpha1.AgentClass
		err := l.deps.K8sClient.Get(ctx, types.NamespacedName{Namespace: ch.Namespace, Name: name}, &ac)
		switch {
		case err == nil:
			class = &ac
			// Cross-agent admission rides this same read: one GET a minute
			// answers both questions, and resolving them from one object is
			// what keeps the flag and the budget describing the same class.
			l.setCrossAgent(&ac)
		case apierrors.IsNotFound(err):
			logger.Info("slack listener: bound AgentClass not found; requiring baseline scopes only",
				"channel", ch.Name, "agentClass", name)
			// A DELETED class must stop admitting other agents. The scope half
			// fails safe by falling back to the baseline; the admission half
			// fails safe by closing, and the two directions are opposite on
			// purpose — the risk here is admitting, not refusing.
			l.setCrossAgent(nil)
		default:
			logger.Info("slack listener: reading the bound AgentClass failed; keeping the last resolved scopes",
				"channel", ch.Name, "agentClass", name, "err", err.Error())
			l.scopesMu.Lock()
			last := l.scopeFeatures
			l.scopesMu.Unlock()
			if last != nil {
				return last // do not flap the condition on a transient read error
			}
			return channelfeatures.Baseline()
		}
	}

	features, err := channelfeatures.ActiveFor(class)
	if err != nil {
		// ActiveFor already failed closed on the malformed capability and
		// returned what it could resolve. Surface the reason: an agent whose
		// capability JSON is broken silently loses the scopes that capability
		// needed, and this log is the only place that says why.
		logger.Info("slack listener: resolving the agent's capabilities reported a problem",
			"channel", ch.Name, "err", err.Error())
	}
	l.cacheFeatures(name, features)
	return features
}

// requiredScopes resolves the bot-token scopes ch actually needs, from the
// capabilities of the AgentClass it is bound to. An agent that never attaches a
// file does not need files:write, and must not be flagged for it.
func (l *slackListener) requiredScopes(ctx context.Context, ch *spiceboxv1alpha1.Channel) []string {
	return channelkinds.ScopesFor(&Kind{}, l.activeFeatures(ctx, ch))
}

// refreshScopesValid recomputes the ScopesValid condition from the cached
// granted-scope header and the CURRENT capabilities of the AgentClass ch binds,
// and keeps that header itself from going stale.
//
// Called from the listener's connect path and again on channelManager's
// existing 5s reconcile tick, so editing an agent's capabilities updates the
// condition rather than leaving the connect-time answer stale. channelsd is not
// controller-runtime, so there is no watch to add — the tick already runs.
//
// BOTH inputs can change while the listener is connected, and only one of them
// lives in the cluster. The capabilities are re-read from the API server;
// what the bot token is granted is re-read from Slack, throttled, off this
// goroutine — see scheduleGrantedScopeRecheck. Recomputing from the cache
// alone made the condition's own remedy ("add the scope, re-install the app")
// impossible to satisfy, since a re-install changes nothing in the cluster.
//
// No-ops before the first successful auth.test: with no granted header we
// cannot distinguish "no scopes granted" from "not asked yet", and guessing
// would stamp a false failure onto a healthy Channel.
func (l *slackListener) refreshScopesValid(ctx context.Context, ch *spiceboxv1alpha1.Channel) {
	if ch == nil {
		// Callers always pass the freshly listed object; fall back to the
		// start-time snapshot rather than dereferencing nil.
		ch = l.deps.Channel
	}
	header, known := l.grantedScopes()
	if !known {
		return
	}
	features := l.activeFeatures(ctx, ch)
	missing := missingScopes(header, channelkinds.ScopesFor(&Kind{}, features))
	l.patchScopesValid(ctx, ch, missing, features)
	l.scheduleGrantedScopeRecheck(ctx, len(missing) > 0)
}

// scopesValidState is the ScopesValid condition reduced to the fields this
// listener owns. Compared to decide whether a write is warranted at all;
// LastTransitionTime and ObservedGeneration are deliberately excluded, since
// neither is an input to the answer (which depends on the bot token and the
// agent's capabilities, not on the Channel's own generation).
type scopesValidState struct {
	status  metav1.ConditionStatus
	reason  string
	message string
}

// matches reports whether an existing condition already says exactly this.
func (s scopesValidState) matches(c *metav1.Condition) bool {
	return c != nil && c.Status == s.status && c.Reason == s.reason && c.Message == s.message
}

// patchScopesValid stamps Channel.status.conditions[ScopesValid] from the
// missing scopes and the features that needed them.
//
// It runs on every reconcile tick, so it writes ONLY when the OBJECT disagrees
// with the computed answer — an unconditional patch would bump the Channel's
// resourceVersion every five seconds forever and re-trigger everything watching
// it. The comparison is against ch, the Channel the caller just listed, so the
// steady state costs no API call at all.
//
// Authority is the object, never a memo of what this listener last wrote. That
// distinction is load-bearing: another writer patching status.conditions with a
// merge patch replaces the list wholesale and can revert this condition, and a
// listener that trusted its own memo would never notice and never restore it.
// Comparing against the object each tick makes such a revert self-healing.
//
// When they do disagree, the live Channel is re-read and used as the patch base
// for the same reason in reverse: our own patch must not clobber a condition
// written since ch was listed.
//
// Best-effort: if K8sClient or the Channel are unset (older test harnesses) or
// the patch fails, we log and move on — the scope problem still surfaces at
// message time via the existing "Attachment delivery failed" footer.
func (l *slackListener) patchScopesValid(ctx context.Context, ch *spiceboxv1alpha1.Channel, missing []string, features []channelfeatures.Feature) {
	logger := log.FromContext(ctx)
	if l.deps.K8sClient == nil || ch == nil {
		return
	}

	desired := scopesValidState{
		status: metav1.ConditionTrue,
		reason: spiceboxv1alpha1.ReasonChannelScopesGranted,
	}
	if len(missing) > 0 {
		desired = scopesValidState{
			status:  metav1.ConditionFalse,
			reason:  spiceboxv1alpha1.ReasonChannelMissingScopes,
			message: missingScopeMessage(missing, features),
		}
	}

	if desired.matches(conditions.Find(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionScopesValid)) {
		return
	}

	var live spiceboxv1alpha1.Channel
	key := types.NamespacedName{Namespace: ch.Namespace, Name: ch.Name}
	if err := l.deps.K8sClient.Get(ctx, key, &live); err != nil {
		logger.Info("slack listener: re-reading the Channel to write ScopesValid failed",
			"channel", ch.Name, "err", err.Error())
		return
	}
	// ch can predate a write of our own from earlier in this same tick (Start
	// writes, then the manager lists again next tick); the live object settles it.
	if desired.matches(conditions.Find(live.Status.Conditions, spiceboxv1alpha1.ChannelConditionScopesValid)) {
		return
	}

	if desired.status == metav1.ConditionFalse {
		logger.Info("slack listener: missing bot-token scopes",
			"channel", ch.Name, "missing", missing)
	}
	updated := live.DeepCopy()
	conditions.Set(updated, &updated.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.ChannelConditionScopesValid,
		Status:  desired.status,
		Reason:  desired.reason,
		Message: desired.message,
	})
	if err := l.deps.K8sClient.Status().Patch(ctx, updated, client.MergeFrom(&live)); err != nil {
		logger.Info("slack listener: patch ScopesValid failed",
			"channel", ch.Name, "err", err.Error())
	}
}

// missingScopeMessage renders the ScopesValid=False message: which scopes are
// missing, what each one costs THIS agent, and where to fix it.
//
// The prose comes from the FeatureRequirement.Degrades of the ACTIVE features
// only — never from every feature the kind declares. Two features can need the
// same scope (channels:history is required by both thread and channel history),
// so building the prose from the whole FeatureSupport map would tell an
// operator that read_channel_history is broken on an agent that never had
// channel history. Features whose requirement is not a scope at all
// (CredentialPortal needs an event subscription) contribute nothing here by
// construction, which is correct: no scope of theirs can go missing.
//
// Sorted, and a pure function of its inputs: this message is patched onto the
// Channel, so map iteration order would make it churn across restarts.
func missingScopeMessage(missing []string, features []channelfeatures.Feature) string {
	sup := (&Kind{}).FeatureSupport()
	seen := map[string]bool{}
	var why []string
	for _, f := range features {
		req, ok := sup[f]
		if !ok || req.Degrades == "" || seen[req.Degrades] {
			continue
		}
		for _, s := range req.Scopes {
			if slices.Contains(missing, s) {
				seen[req.Degrades] = true
				why = append(why, req.Degrades)
				break
			}
		}
	}
	sort.Strings(why)

	msg := "bot token missing required scope(s): " + strings.Join(missing, ", ") + "."
	if len(why) > 0 {
		msg += " Without them: " + strings.Join(why, "; ") + "."
	}
	// The closing sentence is not decoration. The remedy is carried out
	// entirely on Slack's side — the same bot token comes back with more
	// scopes — so an operator who follows it and then watches this condition
	// sit unchanged has no way to tell "not noticed yet" from "did not work",
	// and reaches for a restart. Saying how the condition clears, and how
	// long that takes, is what makes the instruction followable.
	return msg + " Re-install the Slack app at https://api.slack.com/apps → your app →" +
		" 'OAuth & Permissions', add the scope(s), then 'Reinstall to Workspace'." +
		" The new scopes are re-read from Slack and this clears on its own within" +
		" a minute or two — nothing here needs restarting."
}
