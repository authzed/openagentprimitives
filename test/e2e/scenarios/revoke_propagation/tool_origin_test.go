//go:build e2e

// Package revoke_propagation_test is the end-to-end scenario for the
// tool-origin revocation chain.
//
// What this proves end-to-end:
//
//  1. settings.RevokePublisher.Observe detects an explicit allowlist removal and
//     emits a "tool-origin" KindRevoked envelope on the unified ap.revocation
//     NATS subject.
//
//  2. The runner-side revocation.RegisterSubscriber (backed by a
//     revocation.Registry holding a toolorigin.Set) receives, decodes, and
//     scope-filters the envelope, then dispatches to toolorigin.Set.Invalidate.
//
//  3. The hooks.RevocationGuard PreToolCall hook reads the live set and denies
//     calls whose tool origin was revoked.
//
//  4. Scope filtering works: a revoke emitted for a different namespace does NOT
//     land in a subscriber registered for a different namespace.
//
// The integration test (pkg/controllers/settings/revoke_publisher_test.go)
// already covers the emit-rules logic. This scenario promotes the chain to a
// real NATS round-trip: publish → NATS → subscribe → Invalidate → guard Deny.
//
// No real names: alice, team-a, example.com, mcp-github, git-toolkit are all
// fictional per AGENTS.md.
package revoke_propagation_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	settingsctrl "github.com/authzed/openagentprimitives/pkg/controllers/settings"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestToolOriginRevokePropagation is the top-level test body. Subtests
// correspond to the four scenarios described in the package doc.
func TestToolOriginRevokePropagation(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Dial two separate *nats.Conn against the harness's embedded NATS server:
	// one for the operator-side publisher, one for the runner-side subscriber.
	// Mirrors internal/cmd/operator (publish) and internal/cmd/runner (subscribe) split.
	pubConn, err := nats.Connect(h.NATSURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.Name("e2e-tool-origin-revoke-pub"))
	require.NoError(t, err, "dial publisher NATS conn")
	t.Cleanup(func() {
		if err := pubConn.Drain(); err != nil {
			t.Logf("publisher conn drain on cleanup: %v", err)
		}
	})

	subConn, err := nats.Connect(h.NATSURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.Name("e2e-tool-origin-revoke-sub"))
	require.NoError(t, err, "dial subscriber NATS conn")
	t.Cleanup(func() {
		if err := subConn.Drain(); err != nil {
			t.Logf("subscriber conn drain on cleanup: %v", err)
		}
	})

	// Runner-side: build a toolorigin.Set + Registry and register the
	// subscriber for namespace "team-a".
	set := toolorigin.New()
	reg := revocation.NewRegistry()
	require.NoError(t, reg.Register(set), "register toolorigin invalidator")
	require.NoError(t,
		revocation.RegisterSubscriber(ctx, &natsSubscriberAdapter{conn: subConn}, reg, "team-a"),
		"RegisterSubscriber")
	// Flush the subscription to the NATS server before publishing.
	require.NoError(t, subConn.Flush(), "flush subscriber connection")

	// Operator-side: build settings.RevokePublisher backed by the real NATS conn.
	rp := settingsctrl.NewRevokePublisher(
		revocation.NewPublisher(&natsEnvelopePublisher{nc: pubConn}),
	)

	// Wire the RevocationGuard with a static origin map:
	//   "gh_tool"  → "mcpserver/mcp-github"
	//   "jira_tool" → "mcpserver/mcp-jira"
	//   "git_tool"  → "toolkit/git-toolkit"
	//   "aws_tool"  → "toolkit/aws"
	originMap := map[string]string{
		"gh_tool":   "mcpserver/mcp-github",
		"jira_tool": "mcpserver/mcp-jira",
		"git_tool":  "toolkit/git-toolkit",
		"aws_tool":  "toolkit/aws",
	}
	g := hooks.NewRevocationGuard(hooks.RevocationGuardDeps{
		Set: set,
		LookupOrigin: func(name string) string {
			return originMap[name]
		},
	})

	t.Run("Scenario A: MCP server drop propagates to set", func(t *testing.T) {
		// The publisher reads its previous observation from, and stamps it onto,
		// the CR status the reconciler would be writing. One status per crKey,
		// reused across Observes, is what a real CR across reconciles looks like.
		status := &spiceboxv1alpha1.SettingsStatus{}
		mcpPrime := []spiceboxv1alpha1.AllowedMCPServer{{Name: "mcp-github"}, {Name: "mcp-jira"}}
		mcpDrop := []spiceboxv1alpha1.AllowedMCPServer{{Name: "mcp-jira"}}

		// Prime: first Observe establishes the baseline — no emit.
		rp.Observe(ctx, &mcpPrime, nil, status, "team-a/settings-a", "team-a")

		// Drop mcp-github: second Observe emits a revoke for mcpserver/mcp-github.
		rp.Observe(ctx, &mcpDrop, nil, status, "team-a/settings-a", "team-a")

		assert.Eventually(t, func() bool {
			return set.IsRevoked("mcpserver/mcp-github")
		}, 5*time.Second, 100*time.Millisecond,
			"mcpserver/mcp-github must be revoked within 5s of drop")

		assert.False(t, set.IsRevoked("mcpserver/mcp-jira"),
			"mcpserver/mcp-jira must NOT be revoked (still in allowlist)")
	})

	t.Run("Scenario B: toolkit drop propagates to set", func(t *testing.T) {
		status := &spiceboxv1alpha1.SettingsStatus{}
		tkPrime := []string{"git-toolkit", "aws"}
		tkDrop := []string{"aws"}

		// Prime for a distinct crKey so it doesn't share state with scenario A.
		rp.Observe(ctx, nil, &tkPrime, status, "team-a/settings-b", "team-a")

		// Drop git-toolkit: emits revoke for toolkit/git-toolkit.
		rp.Observe(ctx, nil, &tkDrop, status, "team-a/settings-b", "team-a")

		assert.Eventually(t, func() bool {
			return set.IsRevoked("toolkit/git-toolkit")
		}, 5*time.Second, 100*time.Millisecond,
			"toolkit/git-toolkit must be revoked within 5s of drop")

		assert.False(t, set.IsRevoked("toolkit/aws"),
			"toolkit/aws must NOT be revoked (still in allowlist)")
	})

	t.Run("Scenario C: guard denies revoked origin, allows live origin", func(t *testing.T) {
		// At this point scenario A already revoked mcpserver/mcp-github.
		// Ensure the guard reflects it.
		assert.Eventually(t, func() bool {
			return set.IsRevoked("mcpserver/mcp-github")
		}, 5*time.Second, 50*time.Millisecond,
			"prerequisite: mcpserver/mcp-github must be revoked before guard check")

		revokedIn := toolInput("gh_tool")
		d := g.Eval(ctx, revokedIn)
		assert.Equal(t, pipeline.Deny, d.Verdict,
			"guard must Deny gh_tool (mcpserver/mcp-github is revoked)")
		assert.Contains(t, d.Reason, "mcpserver/mcp-github",
			"deny reason must name the revoked origin")

		// mcp-jira is NOT revoked; guard must allow.
		liveIn := toolInput("jira_tool")
		d2 := g.Eval(ctx, liveIn)
		assert.Equal(t, pipeline.Decision{}, d2,
			"guard must allow jira_tool (mcpserver/mcp-jira is NOT revoked)")
	})

	t.Run("Scenario D: scope filter blocks cross-namespace revoke", func(t *testing.T) {
		// Publish a revoke scoped to "team-b" for a distinct origin.
		// The subscriber is registered for "team-a" — it must not see this.
		//
		// Use a second publisher conn to emit directly (without going through
		// settings.RevokePublisher, since Observe always uses the scope passed
		// as the last argument — we drive it directly to test the subscriber
		// scope gate in isolation).
		teamBPrime := []spiceboxv1alpha1.AllowedMCPServer{{Name: "other-server"}}
		teamBDrop := []spiceboxv1alpha1.AllowedMCPServer{}

		// Build a second RevokePublisher scoped to team-b.
		rpB := settingsctrl.NewRevokePublisher(
			revocation.NewPublisher(&natsEnvelopePublisher{nc: pubConn}),
		)

		// Prime then drop — scoped to "team-b".
		statusB := &spiceboxv1alpha1.SettingsStatus{}
		rpB.Observe(ctx, &teamBPrime, nil, statusB, "team-b/settings-d", "team-b")
		rpB.Observe(ctx, &teamBDrop, nil, statusB, "team-b/settings-d", "team-b")

		// Give NATS time to deliver if it were going to (100ms is generous;
		// the subscriber filters synchronously before Invalidate so if it were
		// going to land it would do so quickly).
		time.Sleep(300 * time.Millisecond)

		assert.False(t, set.IsRevoked("mcpserver/other-server"),
			"mcpserver/other-server scoped to team-b must NOT land in team-a's set")
	})
}

// toolInput builds a minimal PreToolCall pipeline.Input for the named tool.
func toolInput(name string) pipeline.Input {
	return pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: name},
	}
}

// ----- local adapter types -------------------------------------------------

// natsEnvelopePublisher implements revocation.EventPublisher backed by a
// *nats.Conn. Mirrors internal/cmd/operator/main.go's natsEnvelopePublisher — inlined
// here to avoid importing cmd/main packages.
type natsEnvelopePublisher struct{ nc *nats.Conn }

func (p *natsEnvelopePublisher) Publish(_ context.Context, env channelevents.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return p.nc.Publish(revocation.Subject, data)
}

// natsSubscriberAdapter adapts a *nats.Conn to revocation.NATSSubscriber.
// Mirrors internal/cmd/runner/nats.go's natsConnAdapter — inlined here to avoid
// importing cmd/main packages.
type natsSubscriberAdapter struct{ conn *nats.Conn }

func (a *natsSubscriberAdapter) Subscribe(subject string, handler func([]byte)) error {
	_, err := a.conn.Subscribe(subject, func(msg *nats.Msg) { handler(msg.Data) })
	return err
}
