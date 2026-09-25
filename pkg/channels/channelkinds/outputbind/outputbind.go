// Package outputbind resolves the dedicated output Channel for a role=input
// Channel, derives the outbound routing anchor from it, and — for an inbound
// that carried no human — derives the line that opens the thread that anchor
// points at. All three are the same question asked at different depths: where
// does this session's reply go, and what is the first thing anyone reads there.
//
// Two consumers share it: channelsd's inbound pipeline (which populates
// AgentSession.spec.outputChannel at session creation) and the Channel
// controller (which reports an unresolvable binding as Valid=False at apply
// time, so a misconfiguration surfaces on kubectl apply rather than when the
// cron fires).
package outputbind

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

var (
	// ErrNoOutputChannel: the AgentClass has no role=output Channel.
	ErrNoOutputChannel = errors.New("no role=output Channel bound to this AgentClass")
	// ErrAmbiguousOutputChannel: more than one role=output Channel matched.
	ErrAmbiguousOutputChannel = errors.New("multiple role=output Channels bound to this AgentClass")
	// ErrKindNoAnchor: the output Channel's kind cannot seed outbound routing.
	ErrKindNoAnchor = errors.New("output Channel's kind provides no outbound anchor")
)

// Resolve returns the single role=output Channel in namespace bound to
// agentClass.
//
// role=both is deliberately NOT a candidate. A role=both Channel is
// self-contained — it is both origin and destination — so treating it as an
// output target would conflate "serves itself" with "serves someone else's
// input", and would risk a cron digest landing in an interactive channel.
func Resolve(ctx context.Context, c client.Client, namespace, agentClass string) (*spiceboxv1alpha1.Channel, error) {
	var list spiceboxv1alpha1.ChannelList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("outputbind: list Channels in %q: %w", namespace, err)
	}
	var found *spiceboxv1alpha1.Channel
	for i := range list.Items {
		ch := &list.Items[i]
		if ch.Spec.AgentClass != agentClass || ch.Spec.Role != spiceboxv1alpha1.ChannelRoleOutput {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: agentClass %q in %q matches %q and %q",
				ErrAmbiguousOutputChannel, agentClass, namespace, found.Name, ch.Name)
		}
		found = ch
	}
	if found == nil {
		return nil, fmt.Errorf("%w: agentClass %q in %q", ErrNoOutputChannel, agentClass, namespace)
	}
	return found, nil
}

// Anchor derives the initial outbound routing key and metadata from ch by
// delegating to its kind's optional OutboundAnchorProvider. Kind-specific
// config (slack's outputDefaults, and any future kind's equivalent) is read
// only inside the kind's own package.
func Anchor(ch *spiceboxv1alpha1.Channel) (string, map[string]string, error) {
	if ch == nil {
		return "", nil, fmt.Errorf("%w: nil Channel", ErrKindNoAnchor)
	}
	k, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return "", nil, fmt.Errorf("%w: kind %q is not registered", ErrKindNoAnchor, ch.Spec.Kind)
	}
	// Type-assert from the registry's interface value; never assign a
	// possibly-nil concrete pointer into an interface.
	provider, ok := k.(channelkinds.OutboundAnchorProvider)
	if !ok {
		return "", nil, fmt.Errorf("%w: kind %q does not implement OutboundAnchorProvider", ErrKindNoAnchor, ch.Spec.Kind)
	}
	// The kind's error names its own destination field; it is carried through
	// verbatim because it is what the operator reading the Channel's condition
	// has to act on.
	key, external, err := provider.OutboundAnchor(ch)
	if err != nil {
		return "", nil, fmt.Errorf("%w: Channel %q (kind %q): %v",
			ErrKindNoAnchor, ch.Name, ch.Spec.Kind, err)
	}
	return key, external, nil
}

// SessionOpening renders the line that opens the outbound thread of a session
// inbound on inputCh under channelKey, and reports whether there is one.
//
// It answers the other half of the question Anchor answers. Anchor says WHERE a
// session's reply goes; for a thread-per-session destination it can only name
// the channel, and the thread root is then whatever lands there first. For a
// human-initiated session that is right by construction — the person's own
// message is the root and already says what the session is about. For a session
// whose inbound carries no human there IS no such message, so the root becomes
// the agent's first output: a plan checklist, a status caption, a bare reply,
// posted into a fresh thread with nothing stating what it is about.
//
// So the two live together: the caller that seeds the anchor also seeds the
// line that will occupy it.
//
// Gated on registry.IsUserlessInput — the one predicate that answers "does this
// Channel's inbound carry a human", already keying several unrelated-looking
// rules. It takes the INPUT Channel, not the class's derived
// status.userlessInput, because the question here is about THIS session's
// origin: a class with both a webhook input and a Slack input has the derived
// bool set, and reading it would put a synthesized opening in front of a human's
// own message on the Slack side.
//
// The sentence itself comes from the trigger's kind (channelkinds.
// TriggerDescriber). Only the kind that wrote channelKey can read it back, and
// only it knows which of its Channel's fields name the thing that fired. A kind
// that does not implement it, or that declines this key, yields ok=false and the
// caller posts nothing — today's behaviour, which is a working thread rather
// than a broken one.
//
// event and body are the verified delivery as it crossed NATS
// (InboundEvent.DeliveryEvent / .RawDelivery) — empty for an inbound that was
// not a webhook — so the kind can say what the triggering thing IS, not merely
// which one it is. See TriggerDescriber for the trust contract on them.
func SessionOpening(inputCh *spiceboxv1alpha1.Channel, channelKey, event string, body []byte) (string, bool) {
	if inputCh == nil || !registry.IsUserlessInput(inputCh) {
		return "", false
	}
	k, ok := registry.Get(inputCh.Spec.Kind)
	if !ok {
		return "", false
	}
	// Type-assert from the registry's interface value; never assign a
	// possibly-nil concrete pointer into an interface.
	describer, ok := k.(channelkinds.TriggerDescriber)
	if !ok {
		return "", false
	}
	text, ok := describer.DescribeTrigger(inputCh, channelKey, event, body)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}
