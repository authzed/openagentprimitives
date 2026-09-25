// The two metaagent IN subjects authzd owns — in.metaagent_request and
// in.metaagent_approval_applied — carry no session identity in their bodies, so
// the NATS SUBJECT is the whole of the routing authority. This file pins the
// accept/reject boundary of that parse, and pins that a body which DOES claim a
// session (an envelope-shaped publish) can never move a request onto a session
// other than the one its subject authorized.
package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// recordingManageScopeChecker records every (ns, name) the gate was asked
// about, as "ns/name". It denies, because these tests are about WHETHER the
// gate is consulted and on which session — never about the answer.
type recordingManageScopeChecker struct {
	mu   sync.Mutex
	seen []string
}

func (c *recordingManageScopeChecker) CheckManageScope(_ context.Context, ns, name string,
	_ identity.CanonicalUserID, _ bool) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, ns+"/"+name)
	return false, nil
}

func (c *recordingManageScopeChecker) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// sessionKeys returns the per-session queue keys the worker has registered.
// A registered key is the observable proof that the subject was accepted:
// Handle creates the entry under w.mu before returning, so no polling.
func sessionKeys(w *MetaagentWorker) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := make([]string, 0, len(w.sessions))
	for k := range w.sessions {
		keys = append(keys, k)
	}
	return keys
}

// TestMetaagentRequestHandler_SubjectIsTheAuthority walks the subjects the
// wildcard subscription can NOT rule out and the ones it can, and requires the
// parse itself to be the gate rather than the subscription string.
//
// "ap.session.*.*.in.metaagent_request" happens to pin the segment token today,
// so a parser that ignores the segment is safe by accident. Accidents do not
// survive a subscription being widened, and the canonical parser
// (pkg/channels/channelevents/envelope.go) refuses both a wrong segment and an empty
// ns/name deliberately — "two parsers that differ only in a segment name must
// not also differ in strictness".
func TestMetaagentRequestHandler_SubjectIsTheAuthority(t *testing.T) {
	body, err := json.Marshal(map[string]any{"requester": "user:alice", "text": "widen scope"})
	require.NoError(t, err, "marshal a metaagent_request body")

	cases := []struct {
		name    string
		subject string
		wantKey string // "" ⇒ the subject must be refused and nothing queued
	}{
		{
			name:    "in.metaagent_request: accepted, queued under the subject's session",
			subject: "ap.session.ns1.sess-a.in.metaagent_request",
			wantKey: "ns1/sess-a",
		},
		{
			name:    "out. segment on an in-only handler: refused, nothing queued",
			subject: "ap.session.ns1.sess-a.out.metaagent_request",
		},
		{
			name:    "empty ns and name tokens: refused, no nameless session queued",
			subject: "ap.session....",
		},
		{
			name:    "empty name token only: refused",
			subject: "ap.session.ns1..in.metaagent_request",
		},
		{
			name:    "wrong prefix: refused",
			subject: "other.session.ns1.sess-a.in.metaagent_request",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := approvedCtx()
			w := NewMetaagentWorker(buildFakeMetaagent(), approval.New(), nil, nil)
			metaagentRequestHandler(ctx, w)(&natsgo.Msg{Subject: tc.subject, Data: body})

			got := sessionKeys(w)
			if tc.wantKey == "" {
				assert.Empty(t, got, "a refused subject must queue nothing")
				return
			}
			assert.Equal(t, []string{tc.wantKey}, got,
				"the queue key must come from the subject, never from the body")
		})
	}
}

// TestMetaagentApprovalRoute_SubjectIsTheAuthority is the same boundary on the
// decision side. A refused subject must never reach the manage_scope gate: the
// gate is keyed on (ns, name), so a subject that parses to the wrong pair would
// authorize the clicker against the wrong session's owner tuple.
func TestMetaagentApprovalRoute_SubjectIsTheAuthority(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"requestId":         "req-1",
		"approved":          true,
		"approverId":        "U1",
		"approverCanonical": "user:alice",
		"action":            "approve",
	})
	require.NoError(t, err, "marshal a metaagent_approval_applied body")

	cases := []struct {
		name      string
		subject   string
		wantCheck string // "ns/name" the gate must be asked about; "" ⇒ never asked
	}{
		{
			name:      "in.metaagent_approval_applied: gate asked about the subject's session",
			subject:   "ap.session.ns1.sess-a.in.metaagent_approval_applied",
			wantCheck: "ns1/sess-a",
		},
		{
			name:    "out. segment on an in-only handler: refused before the gate",
			subject: "ap.session.ns1.sess-a.out.metaagent_approval_applied",
		},
		{
			name:    "empty ns and name tokens: refused before the gate",
			subject: "ap.session....",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := &recordingManageScopeChecker{}
			w := NewMetaagentWorker(buildFakeMetaagent(), approval.New(), nil, nil)
			route := newMetaagentApprovalRoute(w, checker)
			route.handle(approvedCtx(), tc.subject, body)

			if tc.wantCheck == "" {
				assert.Empty(t, checker.asked(), "a refused subject must never reach the manage_scope gate")
				return
			}
			assert.Equal(t, []string{tc.wantCheck}, checker.asked(),
				"the gate must be keyed on the subject-derived session")
		})
	}
}
