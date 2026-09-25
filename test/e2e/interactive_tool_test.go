//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// mockTB records the first Fatalf/Fatal call so we can assert the harness
// reports the expected failure shape without aborting the outer test.
// Implements just the TB methods the harness uses (see scripted_llm.go).
type mockTB struct {
	failed bool
	msg    string
}

func (m *mockTB) Helper() {}
func (m *mockTB) Fatalf(format string, args ...any) {
	if !m.failed {
		m.failed = true
		m.msg = fmt.Sprintf(format, args...)
	}
}

// Fatal is not on the TB interface here but mirrors the testing.TB shape
// in case future harness code reaches for it.
func (m *mockTB) Fatal(args ...any) {
	if !m.failed {
		m.failed = true
		m.msg = fmt.Sprint(args...)
	}
}

func TestScriptedInteractiveTool_EmitInitialAndStdinRules(t *testing.T) {
	f := fake.New()
	tool := NewScriptedInteractiveTool(t)
	tool.EmitInitial(func(e *Emitter) {
		e.Stdout([]byte("READY\n"))
	})
	tool.OnStdin(StdinContains("fix"), func(e *Emitter) {
		e.Stdout([]byte("APPLYING\n"))
	})
	tool.OnStdin(StdinContains("commit"), func(e *Emitter) {
		e.Stdout([]byte("DONE\n"))
		e.Exit(0)
	})
	tool.Install(f, "default/p:c")

	streamer, ok := f.For("default", "p", "c").(exec.StreamingExecutor)
	require.True(t, ok, "fake bound executor must support streaming")
	stream, err := streamer.StreamExec(context.Background(), exec.Request{})
	require.NoError(t, err)

	// Read stdout in a goroutine; collect lines.
	var mu sync.Mutex
	var got []string
	doneRead := make(chan struct{})
	go func() {
		defer close(doneRead)
		buf := make([]byte, 1024)
		for {
			n, rerr := stream.Stdout.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, string(buf[:n]))
				mu.Unlock()
			}
			if rerr == io.EOF {
				return
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Feed two lines.
	go func() {
		_, _ = io.WriteString(stream.Stdin, "please fix the test\n")
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(stream.Stdin, "commit it\n")
		_ = stream.Stdin.Close()
	}()

	res, werr := stream.Wait()
	require.NoError(t, werr)
	assert.Equal(t, int32(0), res.ExitCode)

	<-doneRead
	mu.Lock()
	all := ""
	for _, s := range got {
		all += s
	}
	mu.Unlock()

	assert.Contains(t, all, "READY")
	assert.Contains(t, all, "APPLYING")
	assert.Contains(t, all, "DONE")
	tool.AssertAllRulesConsumed()
}

func TestScriptedInteractiveTool_AssertAllRulesConsumed_DriverNeverRan(t *testing.T) {
	mockT := &mockTB{}
	tool := NewScriptedInteractiveTool(mockT)
	tool.OnStdin(StdinContains("anything"), func(*Emitter) {})
	// Note: no Install call → driver never runs.
	tool.AssertAllRulesConsumed()
	if !mockT.failed {
		t.Fatal("expected AssertAllRulesConsumed to fail when driver never ran")
	}
	if !strings.Contains(mockT.msg, "driver never ran") {
		t.Fatalf("expected 'driver never ran' message, got %q", mockT.msg)
	}
}

func TestScriptedInteractiveTool_AssertAllRulesConsumed_UnfiredRule(t *testing.T) {
	f := fake.New()
	mockT := &mockTB{}
	tool := NewScriptedInteractiveTool(mockT)
	tool.OnStdin(StdinContains("never"), func(*Emitter) {}) // unfired
	tool.Install(f, "default/p:c")

	streamer, ok := f.For("default", "p", "c").(exec.StreamingExecutor)
	require.True(t, ok, "fake bound executor must support streaming")
	stream, err := streamer.StreamExec(context.Background(), exec.Request{})
	require.NoError(t, err)
	_ = stream.Stdin.Close()
	go func() { _, _ = io.Copy(io.Discard, stream.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stream.Stderr) }()
	_, _ = stream.Wait()

	tool.AssertAllRulesConsumed()
	if !mockT.failed {
		t.Fatal("expected AssertAllRulesConsumed to fail with unfired rule")
	}
	if !strings.Contains(mockT.msg, "rule #") {
		t.Fatalf("expected per-rule message, got %q", mockT.msg)
	}
}

func TestFakeCodeyToolkit_Parses(t *testing.T) {
	data, err := os.ReadFile("testdata/fake-codey-toolkit.yaml")
	require.NoError(t, err)
	tk, err := toolkit.LoadBytes(data)
	require.NoError(t, err, "fake-codey-toolkit must parse")
	require.Equal(t, "codey", tk.Name)
	require.Len(t, tk.Subcommands, 1)
	assert.Equal(t, toolkit.SubcommandModeInteractive, tk.Subcommands[0].Mode, "run subcommand must have mode interactive")
}
