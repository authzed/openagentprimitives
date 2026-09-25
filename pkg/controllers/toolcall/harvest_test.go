package toolcall

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// streamingRuntime is a plainRuntime (no native file API, so harvest takes the
// tar-over-exec path) whose Executor is a programmable streaming fake.
type streamingRuntime struct {
	plainRuntime
	ex exec.Executor
}

func (s streamingRuntime) Executor(sandboxkinds.Handle) (exec.Executor, error) { return s.ex, nil }

// TestHarvestOutputs_StderrIsReadWithoutRacingTheCopier — harvestOutputs drains
// the exec stream's stderr on a goroutine nothing ever joins, then reads the
// captured text from the reconcile goroutine on its error paths (a tar.Next
// failure, and a non-zero exit). io.Pipe signals its Read before io.Copy
// performs the matching Write, so the copier's final append can still be in
// flight when Wait() returns — an unsynchronized write/read of the same buffer.
//
// Must be run under -race: without the guard this reports a data race between
// the io.Copy goroutine and harvestOutputs' error formatting. The rest of the
// suite misses it only because no other fixture programs Stderr.
func TestHarvestOutputs_StderrIsReadWithoutRacingTheCopier(t *testing.T) {
	// An end-of-archive-only tar: the read loop finds no members and falls
	// through to the exit-code check, which is where stderr is formatted in.
	var emptyTar bytes.Buffer
	require.NoError(t, tar.NewWriter(&emptyTar).Close(), "build empty tar")

	binder := fake.New()
	binder.ProgramStream("default/demo-pod:sandbox", fake.StreamResponse{
		Stdout:   emptyTar.Bytes(),
		Stderr:   bytes.Repeat([]byte("tar: /work/out: Cannot stat: No such file or directory\n"), 64),
		ExitCode: 2,
	})

	r := &Reconciler{Runtimes: sandboxkinds.Runtimes{
		"demo": streamingRuntime{ex: binder.For("default", "demo-pod", "sandbox")},
	}}
	sess := &spiceboxv1alpha1.SpiceboxSession{ObjectMeta: metav1.ObjectMeta{Name: "sess", Namespace: "default"}}
	sess.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{Kind: "demo", Ref: "demo-pod"}
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ToolCallSpec{CaptureOutputs: []string{"/work/out"}},
	}

	out, err := r.harvestOutputs(context.Background(), sess, tc)
	require.Error(t, err, "a non-zero tar exit must surface as a harvest error")
	assert.Contains(t, err.Error(), "Cannot stat",
		"the captured stderr must reach the error text intact")
	assert.Empty(t, out, "no tar members were emitted")
}
