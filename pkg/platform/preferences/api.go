package preferences

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// SnapshotResponse is the HTTP wire type for user preferences snapshots.
// Subject is the canonical user id the user layer resolved for; empty
// when the turn had no human author (service/webhook/cron sessions).
type SnapshotResponse struct {
	Subject        string   `json:"subject,omitempty"`
	ClassNamespace string   `json:"classNamespace"`
	ClassName      string   `json:"className"`
	Snapshot       Snapshot `json:"snapshot"`
	// Note carries a subject-resolution Reason for a ?user-ref= read that did
	// not resolve (unknown reference form, no linked platform user, subject
	// resolution unavailable on this cluster, …) — empty on a turn-derived
	// read and on a resolved user-ref read. The agent relays it verbatim; it
	// is bounded the same way subjectresolve.Resolution.Reason already is.
	Note string `json:"note,omitempty"`
}

// CommitRequest is the HTTP wire type for updating a user preference.
// Value nil (or JSON null) clears the user layer for Key.
// Subject is the verified decider's canonical user id (bare, no "user:").
type CommitRequest struct {
	Key     string         `json:"key"`
	Value   *apiextv1.JSON `json:"value,omitempty"`
	Subject string         `json:"subject"`
}
