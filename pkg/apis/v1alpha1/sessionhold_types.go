package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// SessionHold parks an AgentSession for forensic review and gates its return to
// running on a human decision.
//
// Shaped after CredentialUpdateRequest deliberately: spec.sessionRef is 1:1, so
// its mapper is a pure name projection needing no List, and the owner-ref to the
// AgentSession makes the hold live exactly as long as the session it holds.
//
// This CR's controller NEVER writes AgentSession.status.phase. Phase is owned by
// the AgentSession reconciler alone; co-ownership by two operator reconcilers is
// the hazard credentialupdate.go documents.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Session",type=string,JSONPath=`.spec.sessionRef.name`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type SessionHold struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SessionHoldSpec   `json:"spec,omitempty"`
	Status SessionHoldStatus `json:"status,omitempty"`
}

type SessionHoldSpec struct {
	// SessionRef is the AgentSession this hold parks. The CR also carries an
	// owner-ref to it, so the hold is garbage-collected with the session.
	SessionRef NamespacedRef `json:"sessionRef"`

	// Reason is platform-authored trip text shown on the release card. It is
	// never agent-authored: on this card the agent is the SUBJECT of the
	// decision, not the requester, so it gets no voice.
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason"`

	// Source is "manual" or "tripper/<name>". It exists for the audit trail and
	// the card's wording; nothing branches on it.
	// +kubebuilder:validation:MaxLength=128
	Source string `json:"source"`

	// TrippedBy is the canonical subject of the human who tripped it. Empty for
	// an automated trip, which is identified by Source instead.
	// +optional
	TrippedBy identity.Subject `json:"trippedBy,omitempty"`
}

type SessionHoldStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is Active or Released.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Determination is platform-authored text explaining the current phase,
	// surfaced rather than logged so a refusal is never silent.
	//
	// WRITTEN ONLY BY THE SESSIONHOLD CONTROLLER, which owns the hold's
	// DISPOSITION: what was decided about it — released, refused, cascaded to
	// descendants, or a cascade that failed. See Containment for the other
	// half and why they are two fields.
	// +optional
	Determination string `json:"determination,omitempty"`

	// Containment is platform-authored text explaining what happened to the
	// SESSION as it was parked: that it is held, and whether the workspace
	// snapshot that preserves the evidence succeeded.
	//
	// WRITTEN ONLY BY THE AGENTSESSION RECONCILER.
	//
	// It is a separate field from Determination because the two narratives are
	// INDEPENDENT, not alternatives. A workspace snapshot can fail while a
	// cascade to descendants also fails, and both are true at once. Sharing one
	// field made each writer erase the other's report — so an operator
	// debugging a doubly-degraded hold saw only whichever controller wrote
	// last, in exactly the state where they need both. Two writers on one
	// free-text status field also woke each other's watches on every pass; the
	// loop was the symptom, the shared field was the disease.
	//
	// Splitting is necessary but not sufficient: each writer must ALSO skip
	// the write when the text is unchanged, or the two keep waking each other
	// through the same object even though neither is erasing anything.
	// +optional
	Containment string `json:"containment,omitempty"`

	// TrippedAt is when the controller first observed this hold. An OBSERVATION,
	// set once: it must never live in spec, or a re-apply stops being a no-op.
	// +optional
	TrippedAt *metav1.Time `json:"trippedAt,omitempty"`

	// SnapshotHandle names the workspace snapshot taken at trip time, empty
	// until the snapshot Job completes.
	// +optional
	SnapshotHandle string `json:"snapshotHandle,omitempty"`

	// InteractionRef is the requestRef of the published release card.
	// +optional
	InteractionRef string `json:"interactionRef,omitempty"`

	// ReleasedBy is the canonical subject of the human who approved the release
	// card, set once by the SessionHold controller's Decide alongside
	// Phase=Released. The AgentSession reconciler reads it back to attribute the
	// lifecyclecore.Released event it emits when it observes this hold released.
	// +optional
	ReleasedBy identity.Subject `json:"releasedBy,omitempty"`
}

const (
	SessionHoldPhaseActive   = "Active"
	SessionHoldPhaseReleased = "Released"
)

// LabelCascadeOf names the ORIGINATING SessionHold a cascaded hold was fanned
// out from. Set only on a hold this controller's own cascade created for a
// descendant of the originating hold's session (pkg/controllers/sessionhold's
// cascadeHold) — a directly-created hold (manual, or a tripper's) never
// carries it.
//
// It exists so a release can find and clear exactly the holds IT created: a
// cascaded hold is owner-ref'd to its own child session, not to the
// originating hold, so k8s garbage collection never removes it just because
// the originating hold is released — only this label lets releaseCascade
// (pkg/controllers/sessionhold) discover and release them together. Without
// it, cascading a hold across a delegation subtree would be a one-way door:
// every descendant frozen, and nothing left that knows which holds a single
// release should clear.
const LabelCascadeOf = "agentprimitives.authzed.com/cascade-of"

// IsReleased reports whether this hold has been cleared by a human.
func (h *SessionHold) IsReleased() bool {
	return h.Status.Phase == SessionHoldPhaseReleased
}

// +kubebuilder:object:root=true
type SessionHoldList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SessionHold `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SessionHold{}, &SessionHoldList{})
}
