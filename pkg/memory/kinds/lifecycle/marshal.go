package lifecycle

import (
	"encoding/json"
	"fmt"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
)

// envelope is the on-disk wire format for typed lifecycle events. Nesting the
// event (rather than flat-merging it) keeps the discriminator legible in raw
// JSON and avoids field-name clashes. Because the envelope IS the entry Content,
// every field here is covered by the signed EntryDigest, so carrying the order
// key needs no signing or chain change.
type envelope struct {
	// Type is the stable wire-type name from typeNameOf; the decode discriminator.
	Type string `json:"type"`
	// Event is the concrete event's own JSON, decoded per Type.
	Event json.RawMessage `json:"event"`
	// Order is the cross-publisher fold-ordering key; nil on a legacy entry,
	// which decodes to a zero OrderKey and folds by createdAt instead.
	Order *OrderKey `json:"order,omitempty"`
}

// Marshal encodes a lifecycle transition Event as a type-discriminated JSON
// envelope with no ordering key stamped (legacy/unstamped form). Callers that
// hold an ordering key use MarshalWithOrder.
func Marshal(ev lc.Event) ([]byte, error) {
	return MarshalWithOrder(ev, OrderKey{})
}

// MarshalWithOrder encodes ev plus its cross-publisher ordering key. The key is
// written only when it is stamped (a real region present); an unstamped key is
// omitted entirely. Returns an error for unknown event types so callers detect
// schema drift at write time rather than silently dropping transitions.
func MarshalWithOrder(ev lc.Event, key OrderKey) ([]byte, error) {
	typ, err := typeNameOf(ev)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("lifecycle.Marshal: encoding %s payload: %w", typ, err)
	}
	env := envelope{Type: typ, Event: json.RawMessage(payload)}
	if key.stamped() {
		k := key
		env.Order = &k
	}
	return json.Marshal(env)
}

// Unmarshal decodes a JSON envelope produced by Marshal back to the concrete
// lifecycle.Event, discarding the ordering key. Unknown type strings surface as
// errors so callers detect forward-compatibility gaps rather than silently
// dropping transitions.
func Unmarshal(data []byte) (lc.Event, error) {
	ev, _, err := decodeEnvelope(data)
	return ev, err
}

// decodeEnvelope decodes a JSON envelope to its concrete event and its ordering
// key. A legacy envelope (no "order") yields a zero OrderKey, which folds by
// createdAt.
func decodeEnvelope(data []byte) (lc.Event, OrderKey, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, OrderKey{}, fmt.Errorf("lifecycle.Unmarshal: decoding envelope: %w", err)
	}
	ev, err := decodeEvent(env.Type, env.Event)
	if err != nil {
		return nil, OrderKey{}, fmt.Errorf("lifecycle.Unmarshal: %w", err)
	}
	var key OrderKey
	if env.Order != nil {
		key = *env.Order
	}
	return ev, key, nil
}

// New returns a ready-to-use Kind. Useful in tests that need to inspect
// the Kind's retention policy without starting the full memory wiring.
func New() Kind { return Kind{} }

// TypeName returns ev's stable wire-type name — the same discriminator
// Marshal writes into the envelope — for a caller that must LABEL an event it
// already holds decoded, rather than re-encode it. Unknown event types error
// for the same reason Marshal does: a transition nobody named must surface as
// schema drift, never as an unlabeled entry.
func TypeName(ev lc.Event) (string, error) { return typeNameOf(ev) }

// typeNameOf maps a concrete Event value to its stable wire type string.
func typeNameOf(ev lc.Event) (string, error) {
	switch ev.(type) {
	// provisioning
	case lc.SettingsAccepted:
		return "settings_accepted", nil
	case lc.PodReady:
		return "pod_ready", nil
	case lc.Unschedulable:
		return "unschedulable", nil
	case lc.ProvablyUnschedulable:
		return "provably_unschedulable", nil
	case lc.RunnerPodRefused:
		return "runner_pod_refused", nil
	// credentials
	case lc.CredsMissing:
		return "creds_missing", nil
	case lc.CredsLinked:
		return "creds_linked", nil
	case lc.CredsTimeout:
		return "creds_timeout", nil
	// identity choice (ask|dynamic)
	case lc.IdentityChoicePending:
		return "identity_choice_pending", nil
	case lc.IdentityChoiceResolved:
		return "identity_choice_resolved", nil
	case lc.IdentityChoiceCancelled:
		return "identity_choice_cancelled", nil
	case lc.IdentityChoiceTimeout:
		return "identity_choice_timeout", nil
	// authority handoff
	case lc.RunnerClaimed:
		return "runner_claimed", nil
	case lc.RunnerTerminal:
		return "runner_terminal", nil
	case lc.RunnerCrash:
		return "runner_crash", nil
	case lc.Stopped:
		return "stopped", nil
	// turn
	case lc.TurnCompleted:
		return "turn_completed", nil
	case lc.AgentWorkComplete:
		return "agent_work_complete", nil
	// hooks
	case lc.HookDeny:
		return "hook_deny", nil
	case lc.HookHalt:
		return "hook_halt", nil
	// decisions
	case lc.DecisionAsked:
		return "decision_asked", nil
	case lc.DecisionResolved:
		return "decision_resolved", nil
	// recovery / conversational
	case lc.ProviderError:
		return "provider_error", nil
	case lc.RetryRequested:
		return "retry_requested", nil
	case lc.RetryTTLExpired:
		return "retry_ttl_expired", nil
	case lc.WakeRequested:
		return "wake_requested", nil
	case lc.ArchiveSweep:
		return "archive_sweep", nil
	case lc.Expired:
		return "expired", nil
	case lc.Sleep:
		return "sleep", nil
	// await within Running
	case lc.AwaitYieldEntered:
		return "await_yield_entered", nil
	case lc.AwaitResumed:
		return "await_resumed", nil
	case lc.IdleYield:
		return "idle_yield", nil
	case lc.ShareDeniedYield:
		return "share_denied_yield", nil
	// in-flight (recorded, no phase change)
	case lc.Revoked:
		return "revoked", nil
	case lc.ScopeMutated:
		return "scope_mutated", nil
	case lc.RestartRequested:
		return "restart_requested", nil
	// forensic hold
	case lc.Held:
		return "held", nil
	case lc.Released:
		return "released", nil
	default:
		return "", fmt.Errorf("lifecycle.Marshal: unknown event type %T", ev)
	}
}

// decodeEvent unmarshals the raw event bytes into the concrete type named by typ.
// Each case calls jsonInto before the return so that the populated value is
// captured — evaluating ev in the return statement before jsonInto runs would
// return the zero value.
func decodeEvent(typ string, raw json.RawMessage) (lc.Event, error) {
	switch typ {
	case "settings_accepted":
		var ev lc.SettingsAccepted
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "pod_ready":
		var ev lc.PodReady
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "unschedulable":
		var ev lc.Unschedulable
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "provably_unschedulable":
		var ev lc.ProvablyUnschedulable
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "runner_pod_refused":
		var ev lc.RunnerPodRefused
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "creds_missing":
		var ev lc.CredsMissing
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "creds_linked":
		var ev lc.CredsLinked
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "creds_timeout":
		var ev lc.CredsTimeout
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "identity_choice_pending":
		var ev lc.IdentityChoicePending
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "identity_choice_resolved":
		var ev lc.IdentityChoiceResolved
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "identity_choice_cancelled":
		var ev lc.IdentityChoiceCancelled
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "identity_choice_timeout":
		var ev lc.IdentityChoiceTimeout
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "runner_claimed":
		var ev lc.RunnerClaimed
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "runner_terminal":
		var ev lc.RunnerTerminal
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "runner_crash":
		var ev lc.RunnerCrash
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "stopped":
		var ev lc.Stopped
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "turn_completed":
		var ev lc.TurnCompleted
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "agent_work_complete":
		var ev lc.AgentWorkComplete
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "hook_deny":
		var ev lc.HookDeny
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "hook_halt":
		var ev lc.HookHalt
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "decision_asked":
		var ev lc.DecisionAsked
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "decision_resolved":
		var ev lc.DecisionResolved
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "provider_error":
		var ev lc.ProviderError
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "retry_requested":
		var ev lc.RetryRequested
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "retry_ttl_expired":
		var ev lc.RetryTTLExpired
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "wake_requested":
		var ev lc.WakeRequested
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "archive_sweep":
		var ev lc.ArchiveSweep
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "expired":
		var ev lc.Expired
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "sleep":
		var ev lc.Sleep
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "await_yield_entered":
		var ev lc.AwaitYieldEntered
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "await_resumed":
		var ev lc.AwaitResumed
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "idle_yield":
		var ev lc.IdleYield
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "share_denied_yield":
		var ev lc.ShareDeniedYield
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "revoked":
		var ev lc.Revoked
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "scope_mutated":
		var ev lc.ScopeMutated
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "restart_requested":
		var ev lc.RestartRequested
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "held":
		var ev lc.Held
		err := jsonInto(raw, &ev, typ)
		return ev, err
	case "released":
		var ev lc.Released
		err := jsonInto(raw, &ev, typ)
		return ev, err
	default:
		return nil, fmt.Errorf("unknown event type %q", typ)
	}
}

// jsonInto is a decode helper that wraps Unmarshal errors with the type name
// so callers can identify which event caused a decode failure.
func jsonInto(raw json.RawMessage, dst any, typ string) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decoding %s: %w", typ, err)
	}
	return nil
}
