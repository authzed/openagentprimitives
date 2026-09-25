// tools_close.go implements close_others: the one tool a builder has for
// ending the person's OTHER open workshops, so they can start a new build when
// they are already at their limit.
//
// The tool never closes anything itself. It writes a REQUEST onto its own
// Workshop CR (spec.closeRequests, the same grant request_install writes
// through) and reads the operator's answers back off status.closeRequests. The
// decision is the operator's, checked per target against the person this
// builder acts for — see pkg/controllers/workshop/close.go.
//
// Two shapes of request, because the two sides see different things. Named
// targets are written one entry each. "All of mine" cannot be: this sidecar's
// Role is name-restricted to its own Workshop, so it cannot list the person's
// workshops and has nothing to name — it writes a wildcard target
// (WorkshopCloseTargetAll) instead and lets the controller, which can list,
// resolve it. That ask's own status entry is the tool's signal that the
// expansion finished; the per-target entries carrying its key are the answer.
//
// A person may ask for "all of mine" more than once, so a call made once the
// last ask has been ANSWERED writes a key of its own — "*", then "*:2", "*:3"
// — and the controller expands each afresh onto the workshops no request has
// settled yet (nextWildcardKey). The call reports the entries stamped with the
// key it is waiting on and nothing else: another ask's answer is not this
// one's, and handing it back would show a person a list that stopped being
// true when they opened their next workshop.
//
// A call made while an ask is STILL OUTSTANDING writes nothing and waits for
// that ask instead (recordWildcardAsk). "Check again with the same call" is
// what the skills tell a person to do while they wait, and a fresh ask made
// then would expand onto the workshops no request has settled — none of them —
// and answer "nothing new to close" about the very ask being waited on. What
// the answer cannot cover, closeEarlierAskAnswered and closeEarlierAskPending
// say out loud — one for an ask that landed during the wait, one for an ask
// still open when the wait ran out.
//
// A named target arrives in whichever spelling the person used — the builder
// session's name (what the start-route refusal shows them) or the Workshop's —
// and closeTargetFor maps it to the one a request can carry. The answer is
// spelled back the way they said it. A name that cannot BE a target is refused
// here rather than written (closeTargetNameable): one list, one CRD pattern, so
// an unspellable name would fail the whole write and take the workshops named
// beside it with it. The wildcard's expanded targets were named by nobody, so
// they come back as builder-session names (closeDisplayName): the one spelling
// the person has actually been shown.
//
// Either may carry a namespace in front ("namespace/name") for a workshop
// outside this builder's own, which is how the start-route refusal names one
// and how the operator resolves it. The suffix rule applies to the name half
// alone; the prefix is carried through untouched, in both directions.
package workshopmcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// toolCloseOthers is the MCP tool name announced on the sidecar's surface.
const toolCloseOthers = "close_others"

// closeSelfRefusal is what the tool answers about the workshop it is in. The
// same words the operator would use for the same refusal
// (pkg/controllers/workshop/close.go's closeRefusedSelf), so a person reading
// either one reads the same sentence.
const closeSelfRefusal = "the workshop you are in"

// closeStillDeciding is the honest sentence for an answer that has not arrived
// inside closePollTimeout. Never a guess: the settled decisions are reported as
// settled and everything else is named as outstanding.
const closeStillDeciding = "Still being decided; ask again in a moment."

// closeNotNameable is what the tool answers about a name that cannot BE a
// close target. It points at the one thing that still reaches such a workshop:
// the wildcard resolves every workshop of the person's, including one whose
// name the target grammar cannot spell, so "not found" would be false.
const closeNotNameable = "not a name that can be closed by itself; ask to close them all"

// closeWildcardAsName is what the tool answers when the wildcard itself is
// handed over as a name. It is not one, and the way to reach every other
// workshop is to leave the names out — so the answer says that rather than
// the general "cannot be closed by itself", which would read as a riddle for
// this one input.
const closeWildcardAsName = "not a workshop name; leave the names out to close every other workshop"

// closeEarlierAskAnswered is what a no-names call says when it waited for an
// ask that was ALREADY outstanding rather than making one of its own, and that
// ask landed during the wait. The answer is true and it is theirs — but it was
// decided about the workshops that were open when that ask was made, so a
// person who has opened another one since needs to know this list does not
// cover it, and that asking again now will.
const closeEarlierAskAnswered = "This answers your earlier ask. A workshop opened since then is not covered; ask again to close it."

// closeEarlierAskPending is the same call's answer when the earlier ask is
// STILL open at the end of the wait: nothing has been answered yet, so it
// says what is being waited on and what that wait will not cover, in one
// breath, rather than "still being decided" followed by "this answers".
const closeEarlierAskPending = "Your earlier ask is still being decided; a workshop opened since then is not covered. Ask again in a moment."

// closeWorkshopSuffix is what WorkshopName appends to a builder session's name.
// DERIVED from that function rather than spelled out, so the two cannot drift:
// a target the person gave that already carries it is a Workshop name, and one
// that does not is a builder-session name — which is what the start-route
// refusal shows them, and therefore what they will say back.
var closeWorkshopSuffix = spiceboxv1alpha1.WorkshopName("")

// closePollInterval / closePollTimeout govern how often the tool re-reads its
// own Workshop for the operator's answers, and how long it waits before
// reporting what settled. Package vars, not consts, so a test can shrink them
// rather than waiting out the real window against a fake client that has no
// controller behind it — mirrors applyPollInterval/applyPollTimeout.
var (
	closePollInterval = time.Second
	closePollTimeout  = 30 * time.Second
)

// registerCloseOthers wires close_others onto mcpSrv.
func (s *Server) registerCloseOthers(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolCloseOthers,
		Description: "Close the person's other open builder workshops (all, or the ones named, by the name " +
			"shown on the page or the workshop name). A platform admin may name anyone's. Never closes this " +
			"one. Answers with what was closed, what was refused and why, and anything still being decided.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workshops": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
					"description": "The workshops to close, by the name shown on the page or the workshop " +
						"name — either is understood. Leave this out entirely to close every other workshop " +
						"the person has open.",
				},
			},
		},
	}, s.handleCloseOthers)
}

// closeOthersArgs is close_others' own argument shape. An absent or empty
// Workshops is the wildcard — "every other one of mine" — which is why the
// handler distinguishes "no list given" from "a list that named nothing usable"
// rather than folding the second into the first: widening a mistyped list into
// every workshop the person has is the one mistake this tool must not make.
type closeOthersArgs struct {
	Workshops []string `json:"workshops"`
}

// closeRefusal is one refused target and the operator's plain-words why.
type closeRefusal struct {
	Workshop string `json:"workshop"`
	Why      string `json:"why"`
}

// closeOthersResult is what the model reads back. Every list is non-nil so an
// empty one marshals as [] rather than null — "nothing was closed" and "the
// field is missing" must not look alike to whoever renders this.
type closeOthersResult struct {
	Closed   []string       `json:"closed"`
	Refused  []closeRefusal `json:"refused"`
	NotFound []string       `json:"notFound"`
	Pending  []string       `json:"pending"`
	// Message carries what the lists cannot: the operator's own sentence about
	// a wildcard ask (its counts, "you had no others", or why it was refused),
	// or closeStillDeciding for an answer that had not arrived in time. When
	// the ask was already outstanding when this call was made, it is
	// closeEarlierAskAnswered on the end of the operator's sentence, or
	// closeEarlierAskPending alone if that ask is still open.
	Message string `json:"message,omitempty"`
}

func newCloseOthersResult() *closeOthersResult {
	return &closeOthersResult{
		Closed:   []string{},
		Refused:  []closeRefusal{},
		NotFound: []string{},
		Pending:  []string{},
	}
}

// add files one recorded decision into the list its phase belongs to. An
// unrecognized phase lands in Pending rather than being dropped: the tool does
// not know what it means, and saying "still being decided" about it is the only
// honest thing left to say.
func (r *closeOthersResult) add(entry spiceboxv1alpha1.WorkshopCloseStatus) {
	switch entry.Phase {
	case spiceboxv1alpha1.WorkshopClosePhaseClosed:
		r.Closed = append(r.Closed, entry.Target)
	case spiceboxv1alpha1.WorkshopClosePhaseRefused:
		r.Refused = append(r.Refused, closeRefusal{Workshop: entry.Target, Why: entry.Message})
	case spiceboxv1alpha1.WorkshopClosePhaseNotFound:
		r.NotFound = append(r.NotFound, entry.Target)
	default:
		r.Pending = append(r.Pending, entry.Target)
	}
}

// handleCloseOthers answers the `close_others` tool call: records a close
// request per target (or waits on the one wildcard ask), waits up to
// closePollTimeout for the operator's decisions, and reports them.
//
// Two refusals are made HERE, before anything is written, and both are about a
// name rather than about authority. Its own workshop, by every spelling a
// person might use for it (closeSelfSpellings), and a name the target grammar
// cannot spell (closeTargetNameable). The controller refuses self too, but a
// request that reaches the controller is a request that got recorded, and a
// recorded request is a decision written once and never revised; there is
// nothing to gain by making one that can only be refused. An unspellable name
// never reaches the controller at all — the apiserver would refuse the whole
// write, and the workshops named beside it would go unasked.
func (s *Server) handleCloseOthers(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a closeOthersArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("close_others: decode arguments: %v", err), nil
	}

	sessNS, sessName := s.sessionRef()
	ownWorkshop := spiceboxv1alpha1.WorkshopName(sessName)
	key := client.ObjectKey{Namespace: sessNS, Name: ownWorkshop}
	own := closeSelfSpellings(sessNS, sessName, ownWorkshop)

	result := newCloseOthersResult()
	wildcard := len(a.Workshops) == 0
	var targets []string
	// asked maps a requested target back to the name the person actually used
	// for it, so every list in the answer is spelled the way they said it.
	asked := make(map[string]string, len(a.Workshops))
	seenGiven := make(map[string]struct{}, len(a.Workshops))
	for _, name := range a.Workshops {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seenGiven[name]; dup {
			continue
		}
		seenGiven[name] = struct{}{}
		// Self, by any spelling, before any normalization: the refusal should
		// say back the name they used.
		if _, self := own[name]; self {
			result.Refused = append(result.Refused, closeRefusal{Workshop: name, Why: closeSelfRefusal})
			continue
		}
		// The wildcard is a way of asking, not a workshop: a person who typed
		// it meant "all of them", and the answer says how to ask for that.
		if spiceboxv1alpha1.IsWorkshopCloseWildcard(name) {
			result.Refused = append(result.Refused, closeRefusal{Workshop: name, Why: closeWildcardAsName})
			continue
		}
		target := closeTargetFor(name)
		// A name that cannot be a target is answered about ITSELF, here,
		// rather than written: spec.closeRequests is one list under one CRD
		// pattern, so a single unspellable name fails the whole write and
		// takes the workshops named beside it down with it.
		if !closeTargetNameable(target) {
			result.Refused = append(result.Refused, closeRefusal{Workshop: name, Why: closeNotNameable})
			continue
		}
		// Both spellings of one workshop is one request, and the first spelling
		// is the one the answer uses.
		if _, dup := asked[target]; dup {
			continue
		}
		asked[target] = name
		targets = append(targets, target)
	}

	// A list that named workshops but yielded no target is answered without
	// widening into the wildcard, ever: a person who named workshops asked
	// about those workshops, and "close everything instead" is the one wrong
	// answer here. Only this workshop named — an answer in itself. Nothing
	// usable named — say so rather than record nothing and report success.
	if !wildcard && len(targets) == 0 {
		if len(result.Refused) > 0 {
			return s.jsonResult(result)
		}
		return s.toolErr("close_others: name at least one workshop, or leave the list out to close every other one the person has open"), nil
	}

	// ask is the wildcard key this call is waiting on, and the key its answer
	// comes back under; empty for a named call. earlierAsk says the call is
	// waiting on an ask that was ALREADY outstanding rather than one it wrote.
	var ask string
	var earlierAsk bool
	var requested []string
	var recErr error
	if wildcard {
		ask, earlierAsk, recErr = s.recordWildcardAsk(ctx, key)
		requested = []string{ask}
	} else {
		recErr = s.recordCloseRequests(ctx, key, targets)
		requested = targets
	}
	if recErr != nil {
		if isDeniedErr(recErr) {
			return s.deniedResult(recErr), nil
		}
		return s.toolErr("close_others: recording the request: %v", recErr), nil
	}

	decisions, err := s.awaitCloseDecisions(ctx, key, requested)
	if err != nil {
		return s.toolErr("close_others: the request was recorded, but reading the answer failed: %v", err), nil
	}

	if wildcard {
		// This ask's own entry says its expansion finished; the answer is the
		// entries STAMPED with it. An entry carrying another ask, or none at
		// all, belongs to an earlier "close everything" or to a named call, and
		// was decided about a question this call did not put — reporting it
		// would hand the person a list that stopped being true when they opened
		// their next workshop.
		for i := range decisions {
			if decisions[i].Ask != ask {
				continue
			}
			// Spelled back as the builder-session name the page and the
			// start-route refusal show. Nobody named these — the expansion did
			// — so the workshop name they were decided under is one the person
			// never used and cannot say back.
			entry := decisions[i]
			entry.Target = closeDisplayName(entry.Target)
			result.add(entry)
		}
		summary := closeDecisionFor(decisions, ask)
		switch {
		case summary == nil && earlierAsk:
			result.Message = closeEarlierAskPending
		case summary == nil:
			result.Message = closeStillDeciding
		case earlierAsk:
			// The ask's own sentence first, then which question it answers:
			// the one the person put the first time, about the workshops that
			// were open then.
			result.Message = strings.TrimSpace(summary.Message + " " + closeEarlierAskAnswered)
		default:
			// Whatever phase the operator settled on, its sentence is the only
			// place that part of the answer lives: the counts for a run that
			// closed things, "no other workshops of yours" for a person with
			// nothing left to close, the reason for a refusal. Nothing else in
			// the result carries any of them, so none may be dropped for having
			// a phase this side did not enumerate.
			result.Message = summary.Message
		}
	} else {
		for _, target := range targets {
			given := asked[target]
			entry := closeDecisionFor(decisions, target)
			if entry == nil {
				result.Pending = append(result.Pending, given)
				continue
			}
			// Answered under the name the person gave, not the one the request
			// had to be written under.
			decision := *entry
			decision.Target = given
			result.add(decision)
		}
	}
	if result.Message == "" && len(result.Pending) > 0 {
		result.Message = closeStillDeciding
	}
	return s.jsonResult(result)
}

// closeSelfSpellings is every name that means "the workshop this builder is
// in" — the builder session's name and the Workshop's, each bare and with this
// builder's own namespace in front.
//
// The namespace-qualified pair is there because the start route's refusal
// spells a workshop that way whenever any of the person's workshops sits
// outside the namespace they are starting in, so it is a spelling they are
// shown and will say back. A self-target has exactly one right answer and the
// tool gives it without writing anything: a recorded request is a decision
// written once and never revised, and there is nothing to gain by making one
// that can only be refused.
func closeSelfSpellings(sessNS, sessName, ownWorkshop string) map[string]struct{} {
	return map[string]struct{}{
		sessName:                   {},
		ownWorkshop:                {},
		sessNS + "/" + sessName:    {},
		sessNS + "/" + ownWorkshop: {},
	}
}

// closeNameSegment is the name half of WorkshopCloseRequest.Target's CRD
// pattern: the DNS-label shape a Workshop name and a namespace both have to
// take. Checked here so a name the person made up is answered about by name,
// rather than rejected by the apiserver as a whole-list write failure.
var closeNameSegment = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// closeTargetMaxLength mirrors WorkshopCloseRequest.Target's own
// +kubebuilder:validation:MaxLength. Measured AFTER the suffix rule, because
// the suffix is part of what the request carries.
const closeTargetMaxLength = 127

// closeTargetNameable reports whether a target the tool is about to write can
// be one at all: a name, or a namespace and a name, each of the CRD's shape,
// within the CRD's length. The wildcard is not checked here — it is never a
// name a person gave.
func closeTargetNameable(target string) bool {
	if len(target) > closeTargetMaxLength {
		return false
	}
	prefix, name := splitCloseNamespace(target)
	if prefix != "" && !closeNameSegment.MatchString(strings.TrimSuffix(prefix, "/")) {
		return false
	}
	return closeNameSegment.MatchString(name)
}

// closeTargetFor turns a name the person used into the target a close request
// has to carry — a Workshop's name, with the namespace prefix untouched when
// the person gave one.
//
// The person is shown builder-SESSION names: that is what the start-route
// refusal lists when they are at their limit, and so what they say back when
// they ask for one to be closed. A request has to name the Workshop, and
// WorkshopName is a deterministic suffix, so the two spellings are the same
// fact and either is accepted. A name that already carries the suffix is passed
// through: it is already a Workshop name, and mapping it again would ask about
// a workshop that cannot exist.
//
// A workshop outside this builder's own namespace is shown — and said back — as
// "namespace/name". Only the NAME half is a session name, so only it is mapped;
// the namespace is the operator's to resolve (pkg/controllers/workshop's
// parseCloseTarget).
func closeTargetFor(given string) string {
	prefix, name := splitCloseNamespace(given)
	if strings.HasSuffix(name, closeWorkshopSuffix) {
		return prefix + name
	}
	return prefix + spiceboxv1alpha1.WorkshopName(name)
}

// closeDisplayName is closeTargetFor read the other way: the builder-session
// name behind a Workshop name — keeping any namespace in front of it — which is
// the spelling the page and the start-route refusal show a person. Only the
// wildcard branch needs it: a named call already answers under the name the
// person gave.
func closeDisplayName(target string) string {
	prefix, name := splitCloseNamespace(target)
	return prefix + strings.TrimSuffix(name, closeWorkshopSuffix)
}

// splitCloseNamespace cuts a target at the LAST "/" into the namespace prefix
// (separator included, empty when there is none) and the name after it, so the
// two helpers above can apply the workshop-suffix rule to the name alone.
func splitCloseNamespace(given string) (prefix, name string) {
	i := strings.LastIndex(given, "/")
	if i < 0 {
		return "", given
	}
	return given[:i+1], given[i+1:]
}

// recordCloseRequests appends one spec.closeRequests entry per NAMED target
// that does not already have one, under the same RetryOnConflict + Update
// discipline request_install uses. A target already requested is left exactly
// as it was: the list is keyed by target, and re-stamping an entry would re-open
// a decision the operator writes once — about a workshop that may since have
// been replaced by a later build of the same name.
//
// requestedAt is taken once, outside the retry closure, so a conflict retry
// re-reads the object without restamping what it is about to write.
func (s *Server) recordCloseRequests(ctx context.Context, key client.ObjectKey, targets []string) error {
	requestedAt := metav1.NewTime(time.Now())
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws spiceboxv1alpha1.Workshop
		if err := s.K8s.Get(ctx, key, &ws); err != nil {
			return err
		}
		existing := make(map[string]struct{}, len(ws.Spec.CloseRequests))
		for _, req := range ws.Spec.CloseRequests {
			existing[req.Target] = struct{}{}
		}
		added := false
		for _, target := range targets {
			if _, dup := existing[target]; dup {
				continue
			}
			existing[target] = struct{}{}
			ws.Spec.CloseRequests = append(ws.Spec.CloseRequests, spiceboxv1alpha1.WorkshopCloseRequest{
				Target:      target,
				RequestedAt: requestedAt,
			})
			added = true
		}
		if !added {
			return nil
		}
		return s.K8s.Update(ctx, &ws)
	})
}

// recordWildcardAsk decides which "close every other one" THIS call is waiting
// on, and returns the key its answer comes back under.
//
// A deliberate second ask gets a key of its own (nextWildcardKey), derived
// inside the retry closure from the list the winning attempt actually read: a
// key chosen against a losing read could collide with an ask another call
// wrote in between. A person may ask again — they opened another workshop
// since — and each ask is expanded afresh, so folding one into another would
// hand them the answer to a different question.
//
// But a call made while an ask is STILL OUTSTANDING is not a second ask. It is
// what the skills tell a person to do while they wait ("check again with the
// same call"), and a new ask made then would expand onto the workshops no
// request has settled — which is none of them — and answer "nothing new to
// close" about the very ask being waited on. So that call writes nothing,
// waits for the outstanding ask, and its caller says which question was
// answered (closeEarlierAskAnswered) or is still open (closeEarlierAskPending).
//
// earlier reports exactly that: the returned ask was already standing, and
// nothing was written.
func (s *Server) recordWildcardAsk(ctx context.Context, key client.ObjectKey) (ask string, earlier bool, err error) {
	requestedAt := metav1.NewTime(time.Now())
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws spiceboxv1alpha1.Workshop
		if err := s.K8s.Get(ctx, key, &ws); err != nil {
			return err
		}
		if outstanding := undecidedWildcardAsk(&ws); outstanding != "" {
			ask, earlier = outstanding, true
			return nil
		}
		ask, earlier = nextWildcardKey(ws.Spec.CloseRequests), false
		ws.Spec.CloseRequests = append(ws.Spec.CloseRequests, spiceboxv1alpha1.WorkshopCloseRequest{
			Target:      ask,
			RequestedAt: requestedAt,
		})
		return s.K8s.Update(ctx, &ws)
	})
	if err != nil {
		return "", false, err
	}
	return ask, earlier, nil
}

// undecidedWildcardAsk returns the latest wildcard ask this workshop has asked
// and the controller has not answered — the ask a fresh no-names call waits on
// — or "" when every ask standing in spec has its summary in status.
//
// The ask's own summary entry is what says its expansion FINISHED; the
// per-target entries beside it can arrive over several passes while one target
// is still provisioning, so an ask with decisions and no summary is still
// outstanding.
func undecidedWildcardAsk(ws *spiceboxv1alpha1.Workshop) string {
	outstanding := ""
	for _, req := range ws.Spec.CloseRequests {
		if !spiceboxv1alpha1.IsWorkshopCloseWildcard(req.Target) {
			continue
		}
		if closeDecisionFor(ws.Status.CloseRequests, req.Target) == nil {
			outstanding = req.Target
		}
	}
	return outstanding
}

// nextWildcardKey is the target this workshop's next "close every other one"
// asks under: the plain wildcard for a workshop that has never asked, and the
// sequenced "*:<n>" after that. n counts the wildcard asks already standing and
// is bumped past any key the list already holds, so the answer is an entry of
// its own however the standing keys were numbered — a duplicate target is a
// duplicate map key, and the apiserver refuses the write rather than the
// person's second ask quietly answering with the first one's words.
func nextWildcardKey(requests []spiceboxv1alpha1.WorkshopCloseRequest) string {
	taken := make(map[string]struct{}, len(requests))
	asks := 0
	for _, req := range requests {
		taken[req.Target] = struct{}{}
		if spiceboxv1alpha1.IsWorkshopCloseWildcard(req.Target) {
			asks++
		}
	}
	if asks == 0 {
		return spiceboxv1alpha1.WorkshopCloseTargetAll
	}
	for n := asks + 1; ; n++ {
		candidate := fmt.Sprintf("%s:%d", spiceboxv1alpha1.WorkshopCloseTargetAll, n)
		if _, dup := taken[candidate]; !dup {
			return candidate
		}
	}
}

// awaitCloseDecisions re-reads the workshop until every requested target has a
// status entry, or closePollTimeout elapses. On timeout it returns the
// decisions as they stand rather than an error: a partial answer reported as
// partial is true, and the caller says which targets are still outstanding.
//
// A cancelled caller is NOT a timeout, and returns its error — a tool call the
// runner gave up on must not read as "nothing had been decided yet".
func (s *Server) awaitCloseDecisions(ctx context.Context, key client.ObjectKey, requested []string) ([]spiceboxv1alpha1.WorkshopCloseStatus, error) {
	pollCtx, cancel := context.WithTimeout(ctx, closePollTimeout)
	defer cancel()

	var decisions []spiceboxv1alpha1.WorkshopCloseStatus
	for {
		var ws spiceboxv1alpha1.Workshop
		if err := s.K8s.Get(ctx, key, &ws); err != nil {
			return nil, err
		}
		decisions = ws.Status.CloseRequests
		if closeDecisionsSettled(decisions, requested) {
			return decisions, nil
		}
		select {
		case <-pollCtx.Done():
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return decisions, nil
		case <-time.After(closePollInterval):
		}
	}
}

// closeDecisionsSettled reports whether every requested target has been decided.
func closeDecisionsSettled(decisions []spiceboxv1alpha1.WorkshopCloseStatus, requested []string) bool {
	for _, target := range requested {
		if closeDecisionFor(decisions, target) == nil {
			return false
		}
	}
	return true
}

// closeDecisionFor returns the decision recorded for target, or nil when the
// operator has not made one yet.
func closeDecisionFor(decisions []spiceboxv1alpha1.WorkshopCloseStatus, target string) *spiceboxv1alpha1.WorkshopCloseStatus {
	for i := range decisions {
		if decisions[i].Target == target {
			return &decisions[i]
		}
	}
	return nil
}
