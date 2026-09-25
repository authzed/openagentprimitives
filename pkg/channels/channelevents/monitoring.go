// pkg/channels/channelevents/monitoring.go
//
// MonitoringEvent is a cluster-level framework health report — a token
// that failed to rotate, a controller that can't reconcile, a session
// that crashed, a channel that lost its transport. The operator's
// monitoring watcher publishes one on every status-condition transition;
// channelsd's monitoring relay fans each out to every role=monitoring
// Channel.
//
// Like SessionAttached (session_attached.go) this is intentionally NOT a
// channelevents.Envelope: monitoring events are a one-way, cluster-scoped
// fanout with no per-AgentSession correlate, so Envelope's closed Kind
// enum / version gate / session-required validation do not apply.
package channelevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
)

// MonitoringEventSubject is the fixed NATS subject for framework
// monitoring events. Single publisher (the operator's monitoring
// watcher); single subscriber-side dispatcher (channelsd's relay).
const MonitoringEventSubject = subjects.Monitoring

// Monitoring severity levels. Validate accepts all three; the watcher emits
// only warning and error.
const (
	MonitoringLevelInfo    = "info"
	MonitoringLevelWarning = "warning"
	MonitoringLevelError   = "error"
)

// Monitoring transition values.
const (
	MonitoringTransitionFailed    = "failed"
	MonitoringTransitionRecovered = "recovered"
)

// MonitoringSourceRef identifies the Kubernetes object a MonitoringEvent
// originated from. Validate requires Kind and Name; Namespace is empty for a
// cluster-scoped source.
type MonitoringSourceRef struct {
	Kind      string `json:"kind"` // "AgentIdentity", "AgentSession", …
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// MonitoringEvent is one framework-level failure or recovery report.
type MonitoringEvent struct {
	// Level is the severity: "info" | "warning" | "error". A recovery
	// event carries the SAME level as the failure it resolves so a
	// level filter that surfaced the failure also surfaces its recovery.
	Level string `json:"level"`
	// Category groups the event: "credential" | "reconcile" |
	// "session" | "transport" | "install_request" | "capability_request"
	// (the last two are the workshop handoff cards). Non-exhaustive.
	Category string `json:"category"`
	// Transition is "failed" (entered the failure state) or "recovered".
	Transition string `json:"transition"`
	// Source is the originating Kubernetes object.
	Source MonitoringSourceRef `json:"source"`
	// Condition is the status-condition type that transitioned (e.g.
	// "Refresh", "Failed", "Connected").
	Condition string `json:"condition"`
	// Reason is the condition's machine reason (e.g. "TokenEndpointError").
	Reason string `json:"reason,omitempty"`
	// Summary is the condition's human-readable message.
	Summary string `json:"summary,omitempty"`
	// Hint is optional operator-facing remediation guidance.
	Hint string `json:"hint,omitempty"`
	// Timestamp is when the underlying condition transitioned.
	Timestamp time.Time `json:"timestamp"`
}

// Validate rejects structurally incomplete events at the publish and
// receive boundaries.
func (e MonitoringEvent) Validate() error {
	switch e.Level {
	case MonitoringLevelInfo, MonitoringLevelWarning, MonitoringLevelError:
	default:
		return fmt.Errorf("invalid level %q", e.Level)
	}
	switch e.Transition {
	case MonitoringTransitionFailed, MonitoringTransitionRecovered:
	default:
		return fmt.Errorf("invalid transition %q", e.Transition)
	}
	if e.Category == "" {
		return errors.New("category is required")
	}
	if e.Source.Kind == "" || e.Source.Name == "" {
		return errors.New("source.kind and source.name are required")
	}
	if e.Condition == "" {
		return errors.New("condition is required")
	}
	return nil
}

// PublishMonitoring validates ev, JSON-encodes it, and publishes it on
// MonitoringEventSubject via the supplied PublishFunc.
func PublishMonitoring(publish PublishFunc, ev MonitoringEvent) error {
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("monitoring event: %w", err)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal monitoring event: %w", err)
	}
	return publish(MonitoringEventSubject, data)
}
