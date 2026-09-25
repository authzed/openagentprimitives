//go:build e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	agentsession "github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestInProcessFactory_StartStop_NoLeak smoke-tests the Start/Stop
// lifecycle without driving any conversation. Verifies:
//   - Start returns nil
//   - Start is idempotent (second call is a no-op)
//   - Stop returns nil and the goroutine exits within 5s
//   - Stop is idempotent (second call is a no-op)
//
// Implementation note on the scripted rule: the smoke test registers a
// repeating EndTurn rule so that if the runner goroutine wins the race
// and calls Provider.Send before Stop fires the cancel, ScriptedLLM has
// something to match against. Without the rule, an unmatched Send would
// call t.Fatalf — which from a non-test goroutine is silently dropped by
// the Go test runtime but still indicates a bug in the test setup.
func TestInProcessFactory_StartStop_NoLeak(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	f := &e2e.InProcessRunnerFactory{LLM: scripted}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "test-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    1,
				MaxTokens:   100,
				MaxDuration: metav1.Duration{Duration: 5 * time.Second},
			},
		},
	}

	ctx := context.Background()
	require.NoError(t, f.Start(ctx, sess, class, agentsession.StartOpts{}))
	require.NoError(t, f.Start(ctx, sess, class, agentsession.StartOpts{}), "Start must be idempotent")

	require.NoError(t, f.Stop(ctx, sess))
	assert.NoError(t, f.Stop(ctx, sess), "Stop must be idempotent")
}

// A test that finishes without calling Stop — an assertion failed, or the
// harness simply never stopped that session — must not leave its runner
// goroutine turning. The harness registers Shutdown as the per-test cleanup, so
// Shutdown is the last thing standing between one test's runner and the rest of
// the package.
//
// This is not hypothetical. Chasing an e2e failure that reproduced only in the
// full package run, the log showed runners from ALREADY-FINISHED tests still
// advancing turns against their now-dead envtest apiserver:
//
//	best-effort: patch session progress failed
//	  err="Patch https://127.0.0.1:58669/.../centerdot-fake-ac3e40a1/status:
//	       dial tcp 127.0.0.1:58669: connect: connection refused"
//	  turnCount=7
//
// Four such sessions were still looping against seven dead apiservers. They burn
// CPU and contend on the package-shared SpiceDB for the remainder of the run,
// which is what makes later tests miss poll deadlines they clear easily alone.
func TestInProcessFactory_Shutdown_StopsRunnersNeverExplicitlyStopped(t *testing.T) {
	scripted := e2e.NewScriptedLLM(t)
	scripted.OnUserMessage("").Reply(e2e.EndTurn()).Repeating()

	f := &e2e.InProcessRunnerFactory{LLM: scripted}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-leaky", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "test-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are a test agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    1000,
				MaxTokens:   1_000_000,
				MaxDuration: metav1.Duration{Duration: 10 * time.Minute},
			},
		},
	}

	ctx := context.Background()
	require.NoError(t, f.Start(ctx, sess, class, agentsession.StartOpts{}))
	require.True(t, f.IsRunning("default", "sess-leaky"), "precondition: the runner is up")

	// No Stop — straight to Shutdown, exactly as an aborted test would.
	f.Shutdown()

	assert.Eventually(t, func() bool { return !f.IsRunning("default", "sess-leaky") },
		5*time.Second, 20*time.Millisecond,
		"Shutdown must cancel every running runner, not just subscriptions and tool sessions")
}
