//go:build e2e

package threadrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	goalcore "github.com/authzed/openagentprimitives/pkg/agent/goals"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// goalWait bounds every wait on a goal run: occurrences fire on the real clock
// (a schedule's interval is at least a minute), so the harness's default
// timeout is too short for a second occurrence to come due and finish.
const goalWait = 4 * time.Minute

// setGoalAudience scripts the fake Channel's audience before any turn runs.
func setGoalAudience(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	if b.Goals == nil || len(b.Goals.Audience) == 0 {
		return
	}
	subjects := make([]string, 0, len(b.Goals.Audience))
	for _, email := range b.Goals.Audience {
		subjects = append(subjects, e2e.CanonicalForFakeEmail(email).String())
	}
	ch := h.SingleChannel("goals.audience")
	var drv *fakekind.Driver
	require.Eventually(t, func() bool {
		drv = fakekind.DriverFor(ch.Namespace, ch.Name)
		return drv != nil
	}, h.DefaultTimeout(), 50*time.Millisecond, "goals.audience: no fake driver for %s/%s", ch.Namespace, ch.Name)
	drv.SetAudience(subjects)
}

// fakeDrivers returns every fake Channel's driver, the way
// PublishedApprovalPrompts merges their prompts.
func fakeDrivers(h *e2e.Harness) []*fakekind.Driver {
	var channels spiceboxv1alpha1.ChannelList
	if err := h.K8s.List(context.Background(), &channels); err != nil {
		return nil
	}
	var out []*fakekind.Driver
	for i := range channels.Items {
		ch := &channels.Items[i]
		if ch.Spec.Kind != "fake" {
			continue
		}
		if drv := fakekind.DriverFor(ch.Namespace, ch.Name); drv != nil {
			out = append(out, drv)
		}
	}
	return out
}

// decisionOutcome reports whether a click on the card requestRef by clicker
// was applied or refused yet. A refused click has no other observable effect
// (the card stays open, nothing is applied), so the refusal notice the fake
// kind records is how "refused" is told from "not processed yet".
func decisionOutcome(h *e2e.Harness, requestRef, clicker string) (string, bool) {
	for _, drv := range fakeDrivers(h) {
		for _, a := range drv.InteractionApplieds() {
			if a.Payload.RequestRef == requestRef && a.Payload.DecidedBy != nil && strings.EqualFold(string(a.Payload.DecidedBy.Email), clicker) {
				return "applied", true
			}
		}
		for _, r := range drv.InteractionRejections() {
			if r.Payload.RequestRef == requestRef && strings.EqualFold(string(r.Payload.Clicker.Email), clicker) {
				return fmt.Sprintf("rejected (%s: %s)", r.Payload.Class, r.Payload.Reason), true
			}
		}
	}
	return "", false
}

// promptsOf returns the published prompts of one category, in publish order.
func promptsOf(h *e2e.Harness, category string) []fakekind.InteractionPrompt {
	var out []fakekind.InteractionPrompt
	for _, p := range h.PublishedApprovalPrompts() {
		if string(p.Payload.Category) == category {
			out = append(out, p)
		}
	}
	return out
}

// startGoalDecisions makes each bundle decision in order, in the background
// from before the first turn: wait for the card, click as the named person,
// and wait for the outcome the bundle claims. Background, because a card can
// hold a turn open (a plan card on the conversation's own goal writes), and
// the turn's reply is awaited on the test goroutine.
//
// The returned func waits for every decision to be made and reports a click
// that was neither applied nor rejected by name. It runs on the test
// goroutine; the background half only asserts, never stops the test.
func startGoalDecisions(t *testing.T, h *e2e.Harness, b bt.Bundle) func() {
	t.Helper()
	if b.Goals == nil || len(b.Goals.Decisions) == 0 {
		return func() {}
	}
	done := make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		defer close(done)
		for i, d := range b.Goals.Decisions {
			if !makeGoalDecision(t, h, i, d, stop) {
				return
			}
		}
	}()
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(goalWait * time.Duration(len(b.Goals.Decisions))):
			t.Fatalf("goals.decisions: still waiting after %s", goalWait*time.Duration(len(b.Goals.Decisions)))
		}
	}
}

// makeGoalDecision makes one decision and reports whether the next may run.
func makeGoalDecision(t *testing.T, h *e2e.Harness, i int, d bt.Decision, stop <-chan struct{}) bool {
	t.Helper()
	poll := func(timeout time.Duration, cond func() bool) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			select {
			case <-stop:
				return false
			case <-time.After(100 * time.Millisecond):
			}
		}
		return cond()
	}
	var prompt fakekind.InteractionPrompt
	if !assert.True(t, poll(goalWait, func() bool {
		ps := promptsOf(h, d.Category)
		if len(ps) <= d.Nth {
			return false
		}
		prompt = ps[d.Nth]
		return true
	}), "goals.decisions[%d]: card %s #%d was never published", i, d.Category, d.Nth) {
		return false
	}

	if d.Action == "approve" {
		h.ApproveInteraction(prompt, e2e.AsUser(d.As))
	} else {
		h.DenyInteraction(prompt, e2e.AsUser(d.As))
	}

	ref := prompt.Payload.RequestRef
	var outcome string
	if !assert.True(t, poll(h.DefaultTimeout(), func() bool {
		var done bool
		outcome, done = decisionOutcome(h, ref, d.As)
		return done
	}), "goals.decisions[%d]: %s's %s on %s #%d was neither applied nor rejected", i, d.As, d.Action, d.Category, d.Nth) {
		return false
	}
	t.Logf("goals.decisions[%d]: %s's %s on %s #%d: %s", i, d.As, d.Action, d.Category, d.Nth, outcome)
	return assert.True(t, strings.HasPrefix(outcome, d.Want),
		"goals.decisions[%d]: %s's %s on %s #%d: want %s, got %s", i, d.As, d.Action, d.Category, d.Nth, d.Want, outcome)
}

// checkGoalRuns waits for exactly as many occurrences as the bundle asserts on
// to reach a final record, then checks each in due order. Final means a
// proposal and final cost accounting are both recorded: the run reported and
// the dispatcher closed its books.
func checkGoalRuns(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	want := b.Assert.GoalRuns
	if len(want) == 0 {
		return
	}
	ctx := context.Background()
	var runs []goalcore.Occurrence
	done := assert.Eventually(t, func() bool {
		got, err := h.GoalOccurrences(ctx)
		if err != nil {
			t.Logf("goal runs: %v", err)
			return false
		}
		runs = got
		if len(got) < len(want) {
			return false
		}
		for _, o := range got[:len(want)] {
			if o.Proposal == nil || o.Cost == nil || !o.Cost.Final {
				return false
			}
		}
		return true
	}, goalWait, 250*time.Millisecond, "goal runs never all finished")
	if !done {
		raw, err := json.MarshalIndent(runs, "", "  ")
		if err != nil {
			t.Logf("goal runs: encode for the failure log: %v", err)
		}
		t.Logf("goal runs at timeout: %s", raw)
		for _, p := range h.PublishedApprovalPrompts() {
			t.Logf("published card: %s in %s/%s", p.Payload.Category, p.SessionRef.Namespace, p.SessionRef.Name)
		}
		t.FailNow()
	}
	assert.Len(t, runs, len(want), "goal occurrences run")
	prompts := h.PublishedApprovalPrompts()
	for i, w := range want {
		if i >= len(runs) {
			break
		}
		o := runs[i]
		replyState := ""
		if o.Reply != nil {
			replyState = o.Reply.State
		}
		t.Logf("goal run %d (%s, session %s): proposal %s %q, reply %q", i, o.ID, o.SessionName, o.Proposal.Status, o.Proposal.Summary, replyState)
		if w.Proposal != "" {
			assert.Equal(t, w.Proposal, o.Proposal.Status, "goal run %d (%s): proposal status", i, o.ID)
		}
		if w.SummaryContains != "" {
			assert.Contains(t, o.Proposal.Summary, w.SummaryContains, "goal run %d (%s): proposal summary", i, o.ID)
		}
		if w.Reply != "" {
			if assert.NotNil(t, o.Reply, "goal run %d (%s): no reply recorded", i, o.ID) {
				assert.Equal(t, w.Reply, o.Reply.State, "goal run %d (%s): reply state", i, o.ID)
			}
		}
		if w.ApprovalPrompts != nil {
			n := 0
			for _, p := range prompts {
				if p.SessionRef.Name == o.SessionName {
					n++
				}
			}
			assert.Equal(t, *w.ApprovalPrompts, n, "goal run %d (%s): cards published in its session %s", i, o.ID, o.SessionName)
		}
	}
}
