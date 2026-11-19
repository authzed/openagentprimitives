//go:build e2e

package threadrun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// Multi-session bundles: named threads, thread-scoped steps, turn barriers,
// and the admission watch. See pkg/bronzethread/sessions.go for the format.
// A bundle that names no thread takes none of these paths.

// sessionBarrierTimeout bounds every wait on another session's state. Long
// enough for a session to queue behind a whole other session's turn under
// suite contention; short enough that a line that never admits fails by name
// rather than by the suite timeout.
const sessionBarrierTimeout = 2 * time.Minute

// threadKey is the channel key a turn is sent on. Each bundle gets its own
// thread (see Run), and each NAMED thread inside it its own key, so its own
// AgentSession.
func threadKey(b bt.Bundle, thread string) string {
	if thread == "" {
		return "bt-" + b.Name
	}
	return "bt-" + b.Name + "-" + thread
}

// turnSendOptions routes one user turn: its thread, and its Channel when the
// fixture has more than one.
func turnSendOptions(b bt.Bundle, turn bt.UserTurn) []e2e.SendOption {
	opts := []e2e.SendOption{e2e.InThread(threadKey(b, turn.Thread))}
	if turn.Channel != "" {
		opts = append(opts, e2e.OnChannel(turn.Channel))
	}
	return opts
}

// mentionsText reports whether any user text block of req contains text. A
// thread's opening message is in every request its session makes — and, by
// bt.Bundle.Validate, in no other session's.
func mentionsText(req llm.Request, text string) bool {
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && strings.Contains(c.Text, text) {
				return true
			}
		}
	}
	return false
}

// threadSession returns the AgentSession a named thread opened, if it exists.
func threadSession(ctx context.Context, h *e2e.Harness, b bt.Bundle, thread string) (*spiceboxv1alpha1.AgentSession, error) {
	var list spiceboxv1alpha1.AgentSessionList
	if err := h.K8s.List(ctx, &list, client.InNamespace(h.Namespace())); err != nil {
		return nil, err
	}
	key := threadKey(b, thread)
	for i := range list.Items {
		if in := list.Items[i].Spec.InputChannel; in != nil && in.Key == key {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// barrierMet reports whether sess has reached the barrier's state.
func barrierMet(sess *spiceboxv1alpha1.AgentSession, until string) bool {
	if sess == nil {
		return false
	}
	switch until {
	case bt.UntilAwaitingInput:
		return sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseRunning && sess.Status.AwaitingUserInputSince != nil
	case bt.UntilIdle:
		return sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle
	}
	return false
}

// waitForBarriers blocks until every session the turn waits on has reached
// its state.
func waitForBarriers(t *testing.T, h *e2e.Harness, b bt.Bundle, turn bt.UserTurn) {
	t.Helper()
	for _, w := range turn.WaitFor {
		cutoff := time.Now().Add(sessionBarrierTimeout)
		var last *spiceboxv1alpha1.AgentSession
		for {
			sess, err := threadSession(context.Background(), h, b, w.Thread)
			if err == nil {
				last = sess
			}
			if barrierMet(sess, w.Until) {
				break
			}
			if time.Now().After(cutoff) {
				phase := "<no session>"
				if last != nil {
					phase = last.Status.Phase
				}
				t.Fatalf("turn %q waits for thread %q to be %s; it was not within %s (phase %s)",
					turn.Text, w.Thread, w.Until, sessionBarrierTimeout, phase)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// awaitArrival blocks until the session a turn just opened exists, then until
// the wall clock has moved past the second it was created in.
//
// The line orders by creationTimestamp, which the apiserver keeps to the
// second, and breaks a tie on name — which, for a session, is a hash of its
// channel key. Two sessions opened in one second would therefore line up in
// an order unrelated to which arrived first, and a bundle claiming arrival
// order would be claiming a coin toss. Only the bundles with admission on
// pay this.
func awaitArrival(t *testing.T, h *e2e.Harness, b bt.Bundle, thread string) {
	t.Helper()
	cutoff := time.Now().Add(sessionBarrierTimeout)
	for {
		sess, err := threadSession(context.Background(), h, b, thread)
		if err == nil && sess != nil {
			next := sess.CreationTimestamp.Time.Truncate(time.Second).Add(time.Second)
			if wait := time.Until(next); wait > 0 {
				time.Sleep(wait + 10*time.Millisecond)
			}
			return
		}
		if time.Now().After(cutoff) {
			t.Fatalf("thread %q: no AgentSession appeared within %s; the turn never reached the pipeline", thread, sessionBarrierTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// admissionWatch observes every named thread's session for the whole run and
// records what the admission scenario asserts on: when each was first seen
// admitted and first seen finished, how many were admitted at once, and every
// moment a session held part of a pod set it should hold all or none of.
//
// It is a property check the run cannot opt out of: the reconciler creates a
// session's bundle SpiceboxSessions one by one, so the claim is not "a set
// appears atomically" but the two states that MUST be whole — a session that
// was never admitted holds none, and a Running session holds every one.
type admissionWatch struct {
	b    bt.Bundle
	h    *e2e.Harness
	stop context.CancelFunc
	done chan struct{}

	mu            sync.Mutex
	seq           int
	firstAdmitted map[string]int // thread -> observation number
	firstIdle     map[string]int
	peak          int
	violations    []string
	lastErr       error
}

func startAdmissionWatch(t *testing.T, h *e2e.Harness, b bt.Bundle) *admissionWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &admissionWatch{
		b: b, h: h, stop: cancel, done: make(chan struct{}),
		firstAdmitted: map[string]int{}, firstIdle: map[string]int{},
	}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			w.observe(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	t.Cleanup(w.halt)
	return w
}

func (w *admissionWatch) halt() {
	w.stop()
	<-w.done
}

// observe takes one snapshot. A list error skips the snapshot (recorded, and
// reported only if nothing was ever observed): a missed snapshot can only
// lose an observation, never invent a violation.
//
// The lists are not one atomic read, so each claim is checked across a pair
// read in the order that makes it sound (h.K8s is the uncached client, so the
// reads are ordered at the apiserver):
//
//   - "Running holds every bundle" reads sessions, THEN bundles: a session
//     Running at the first read already had its whole set, and nothing
//     reclaims a Running session's set.
//   - "waiting holds no bundle" reads bundles, THEN sessions: a bundle seen at
//     the first read was created after its session was admitted, so the
//     session reads admitted at the second. Read the other way round, a
//     session admitted and provisioned between the two reads would look like
//     a waiting session holding its set.
func (w *admissionWatch) observe(ctx context.Context) {
	ns := client.InNamespace(w.h.Namespace())
	var before, after spiceboxv1alpha1.AgentSessionList
	var boxes spiceboxv1alpha1.SpiceboxSessionList
	var classes spiceboxv1alpha1.AgentClassList
	err := w.h.K8s.List(ctx, &classes, ns)
	if err == nil {
		err = w.h.K8s.List(ctx, &before, ns)
	}
	if err == nil {
		err = w.h.K8s.List(ctx, &boxes, ns)
	}
	if err == nil {
		err = w.h.K8s.List(ctx, &after, ns)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.lastErr = err
		return
	}
	w.seq++

	bundlesOf := map[string]int{}
	for i := range classes.Items {
		bundlesOf[classes.Items[i].Name] = len(classes.Items[i].Spec.ToolBundles)
	}
	held := map[string]int{}
	for i := range boxes.Items {
		if boxes.Items[i].DeletionTimestamp == nil {
			held[boxes.Items[i].Labels["agentprimitives.authzed.com/agentsession"]]++
		}
	}

	prefix := threadKey(w.b, "") + "-"
	threadOf := func(s *spiceboxv1alpha1.AgentSession) (string, bool) {
		if s.Spec.InputChannel == nil || !strings.HasPrefix(s.Spec.InputChannel.Key, prefix) {
			return "", false
		}
		return strings.TrimPrefix(s.Spec.InputChannel.Key, prefix), true
	}

	for i := range before.Items {
		s := &before.Items[i]
		thread, ok := threadOf(s)
		if !ok || s.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseRunning {
			continue
		}
		if n, want := held[s.Name], bundlesOf[s.Spec.Class]; n != want {
			w.violations = append(w.violations, fmt.Sprintf(
				"observation %d: thread %q (%s) is Running with %d of its %d bundle SpiceboxSessions",
				w.seq, thread, s.Name, n, want))
		}
	}

	admittedNow := 0
	for i := range after.Items {
		s := &after.Items[i]
		thread, ok := threadOf(s)
		if !ok {
			continue
		}
		phase := s.Status.Phase
		admitted := admission.IsAdmitted(s)
		terminal := phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded || phase == spiceboxv1alpha1.AgentSessionPhaseFailed
		if admitted && !terminal {
			admittedNow++
			if w.firstAdmitted[thread] == 0 {
				w.firstAdmitted[thread] = w.seq
			}
		}
		if phase == spiceboxv1alpha1.AgentSessionPhaseIdle && w.firstAdmitted[thread] != 0 && w.firstIdle[thread] == 0 {
			w.firstIdle[thread] = w.seq
		}
		booting := phase == "" || phase == spiceboxv1alpha1.AgentSessionPhasePending
		if n := held[s.Name]; booting && !admitted && n > 0 {
			w.violations = append(w.violations, fmt.Sprintf(
				"observation %d: thread %q (%s) is waiting unadmitted yet holds %d of its %d bundle SpiceboxSessions",
				w.seq, thread, s.Name, n, bundlesOf[s.Spec.Class]))
		}
	}
	w.peak = max(w.peak, admittedNow)
}

// waitAllReleased blocks until every named thread's session has finished and
// given its admission back through the sleep reap (status.sleptAt). That is
// the end of the line's story: every session admitted, ran, and freed its room.
func waitAllReleased(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	cutoff := time.Now().Add(sessionBarrierTimeout)
	for {
		var pending []string
		for _, th := range b.Threads() {
			sess, err := threadSession(context.Background(), h, b, th)
			if err != nil || sess == nil || sess.Status.SleptAt == nil {
				pending = append(pending, th)
			}
		}
		if len(pending) == 0 {
			return
		}
		if time.Now().After(cutoff) {
			t.Fatalf("threads %v never finished and released their admission within %s", pending, sessionBarrierTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// checkAdmission asserts the watch's record against the bundle's claims, and
// the properties every admission bundle holds without claiming them: no
// partial set, never more admitted than the capacity, and every thread
// admitted and finished.
func checkAdmission(t *testing.T, w *admissionWatch, b bt.Bundle) {
	t.Helper()
	w.halt()
	w.mu.Lock()
	defer w.mu.Unlock()
	require.NotZero(t, w.seq, "the admission watch never observed the cluster (last error: %v)", w.lastErr)
	t.Logf("admission record over %d observations: admitted %v, completed %v, peak %d admitted at once",
		w.seq, sortedRecord(w.firstAdmitted), sortedRecord(w.firstIdle), w.peak)

	assert.Empty(t, w.violations, "a session held part of a pod set it must hold all or none of")
	assert.LessOrEqual(t, w.peak, b.Admission.CapacitySessions,
		"more sessions were admitted at once than the cluster has room for")
	for _, th := range b.Threads() {
		assert.NotZero(t, w.firstAdmitted[th], "thread %q was never admitted", th)
		assert.NotZero(t, w.firstIdle[th], "thread %q never finished its work after admission", th)
	}

	a := b.Assert.Admission
	if a == nil {
		return
	}
	if a.PeakAdmitted > 0 {
		assert.Equal(t, a.PeakAdmitted, w.peak, "the most sessions admitted at once")
	}
	order := func(kind string, first map[string]int, claims []bt.ThreadOrder) {
		for _, o := range claims {
			f, s := first[o.First], first[o.Then]
			assert.True(t, f != 0 && s != 0 && f < s,
				"%s: thread %q must come strictly before %q (first observed at %d and %d; full record %v)",
				kind, o.First, o.Then, f, s, sortedRecord(first))
		}
	}
	order("admitted", w.firstAdmitted, a.AdmittedBefore)
	order("completed", w.firstIdle, a.CompletedBefore)
}

// sortedRecord renders a first-observation map in observation order, so a
// failed ordering claim shows the order the run actually took.
func sortedRecord(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return m[a] - m[b] })
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s@%d", k, m[k]))
	}
	return out
}

// admissionOptionsFor turns a bundle's admission block into the harness
// option, or nil for every bundle that runs ungated.
func admissionOptionsFor(b bt.Bundle) *e2e.AdmissionOptions {
	if b.Admission == nil {
		return nil
	}
	return &e2e.AdmissionOptions{Capacity: e2e.FixedSetCapacity{Sets: b.Admission.CapacitySessions}}
}

// awaitIdleTTLFor keeps a session parked in await_user_message for as long as
// a barrier may wait on it, for a bundle that waits on one. Every other bundle
// gets zero: await idle-exits at once, the path they were written against.
func awaitIdleTTLFor(b bt.Bundle) time.Duration {
	for _, ut := range b.UserTurns {
		for _, w := range ut.WaitFor {
			if w.Until == bt.UntilAwaitingInput {
				return 2 * sessionBarrierTimeout
			}
		}
	}
	return 0
}
