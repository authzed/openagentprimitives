package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentthread"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/scopeaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// Metaagent holds the wiring needed to handle metaagent requests for
// all sessions. One instance per authzd binary; the per-session
// goroutine (Phase E5) calls into it.
type Metaagent struct {
	Memory    memory.Memory
	Extractor Extractor
	Composer  Composer

	// NoticePublish, when set, delivers a user-facing notice to the channel
	// (channelsd's out.metaagent_notice → ephemeral Slack message). nil =
	// memory-only (notices recorded in metaagent_thread but not delivered).
	NoticePublish func(ctx context.Context, scopeRef memory.Scope, requester, body string) error
}

// SessionRef identifies the agentsession. "namespace/name" form per the spec.
type SessionRef = string

// ApplyScopeChange is the single mutation primitive for scope state.
// See spec §4.1. Hard-deny is enforced ENTIRELY at Layer 2: ApplyDelta records
// HardDeny.Resources into the session_scope Disallow set (and HardDeny.Tools
// into the tool-deny set), which the runner's Scope hook reads at dispatch.
// An empty delta is a no-op.
func (m *Metaagent) ApplyScopeChange(ctx context.Context, scopeRef memory.Scope, _ SessionRef, d scope.ScopeDelta) error {
	if d.IsEmpty() {
		return nil
	}

	// 1. Read current scope.
	cur, _, err := sessionscope.Get(ctx, m.Memory, scopeRef)
	if err != nil {
		return fmt.Errorf("read current scope: %w", err)
	}

	// 2. Compute next scope and write memory. ApplyDelta folds HardDeny into the
	//    Layer-2 Disallow / tool-deny sets — the sole enforcement surface.
	next := scope.ApplyDelta(cur, d, scope.SourceMetaagentApproved, time.Time{})
	// Compare-and-swap on the version read above. The runner read-modify-writes
	// this same document on every dispatch round, and a blind overwrite in
	// either direction silently drops the other's change — here that change is
	// a HardDeny a human just clicked, and Disallow is the sole hard-deny
	// enforcement surface. Refusing is the safe direction: the caller sees an
	// error, and the audit below does not record an application that did not
	// happen.
	if err := sessionscope.PutIfVersion(ctx, m.Memory, scopeRef, next, cur.ScopeVersion); err != nil {
		return fmt.Errorf("write scope: %w", err)
	}

	// 3. Audit.
	if auditErr := scopeaudit.Record(ctx, m.Memory, scopeRef, scopeaudit.Content{
		Delta:     d,
		AppliedAt: time.Now().UTC(),
		Source:    string(scope.SourceMetaagentApproved),
	}); auditErr != nil {
		// Audit failure is best-effort; do not undo the apply.
		slog.Info("metaagent: scope audit record failed", "session", scopeRef.ID, "err", auditErr.Error())
	}

	return nil
}

func (m *Metaagent) notify(ctx context.Context, scopeRef memory.Scope, requesterID, body string) {
	// The append is the durable record of the notice. Nothing below logs its
	// failure (metaagentthread.Append -> auditaccessor.Append returns the error
	// and logs nothing), and in production m.Memory is the signing client — a
	// signing or verify-on-write rejection would otherwise vanish silently.
	// Delivery still proceeds: a notice the user sees beats no notice at all.
	if err := metaagentthread.Append(ctx, m.Memory, scopeRef, metaagentthread.Content{
		Ts:          time.Now().UTC(),
		Role:        metaagentthread.RoleMetaagentNotice,
		Body:        body,
		RequesterID: requesterID,
	}); err != nil {
		slog.Info("metaagent: notice thread append failed (notice still delivered)",
			"session", scopeRef.ID, "requester", requesterID, "err", err.Error())
	}
	if m.NoticePublish != nil {
		if err := m.NoticePublish(ctx, scopeRef, requesterID, body); err != nil {
			slog.Info("metaagent: notice delivery failed (recorded in memory)", "session", scopeRef.ID, "err", err.Error())
		}
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += "• " + l
	}
	return out
}
