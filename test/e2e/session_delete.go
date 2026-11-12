//go:build e2e

package e2e

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// DeleteSession deletes the (ns, name) AgentSession and waits until it is gone
// — finalizer run, CR no longer readable — or fails the test after deadline.
//
// What it leaves behind is the point. The session's memory scope is keyed by
// its name and outlives the CR by design (the transcript is append-only audit
// evidence), so a later inbound on the same channel key finds a transcript and
// no session: the state a person returning to a deleted conversation is in.
func (h *Harness) DeleteSession(ns, name string, deadline time.Duration) {
	h.t.Helper()
	key := client.ObjectKey{Namespace: ns, Name: name}
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := h.K8s.Delete(context.Background(), sess); err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("delete AgentSession %s: %v", key, err)
	}
	cutoff := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(cutoff) {
		err := h.K8s.Get(context.Background(), key, &spiceboxv1alpha1.AgentSession{})
		if apierrors.IsNotFound(err) {
			return
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("AgentSession %s still present %s after delete (its finalizer has not completed; last get err: %v)",
		key, deadline, lastErr)
}
