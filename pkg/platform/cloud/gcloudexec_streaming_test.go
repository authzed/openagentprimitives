package cloud

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGcloudStreaming_StreamsOutputThroughSuspend is the regression for the
// "gcloud output eaten" bug: a long-running gcloud call (the Gateway API enable)
// must stream its stdout AND stderr live to the user, via Suspend so the install
// checklist quiesces, instead of buffering everything and showing nothing for
// minutes.
func TestGcloudStreaming_StreamsOutputThroughSuspend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX sh")
	}
	old := gcloudBin
	t.Cleanup(func() { gcloudBin = old })
	gcloudBin = "sh"

	rep := &stubReporter{interactive: true}
	err := GcloudStreaming(context.Background(), rep, "-c", "echo to-stdout; echo to-stderr 1>&2")
	require.NoError(t, err)
	assert.True(t, rep.suspended, "long gcloud calls must stream via Suspend so the checklist quiesces")
	out := rep.suspendOut.String()
	assert.Contains(t, out, "to-stdout", "stdout must reach the user (was previously buffered/eaten)")
	assert.Contains(t, out, "to-stderr", "stderr (gcloud's own progress) must reach the user too")
}

// TestGcloudStreaming_InheritsSuspendStdin is the regression for the "gcloud
// shows a static line, no spinner, looks hung" bug: GcloudStreaming must wire the
// terminal stdin Suspend lends it to the child's stdin, so gcloud's interactivity
// check sees a TTY. We prove the wiring by having the child `cat` its stdin — the
// bytes only appear if cmd.Stdin was connected to the reader from Suspend.
func TestGcloudStreaming_InheritsSuspendStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX sh")
	}
	old := gcloudBin
	t.Cleanup(func() { gcloudBin = old })
	gcloudBin = "sh"

	rep := &stubReporter{interactive: true, suspendIn: strings.NewReader("from-stdin\n")}
	err := GcloudStreaming(context.Background(), rep, "-c", "cat")
	require.NoError(t, err)
	assert.Contains(t, rep.suspendOut.String(), "from-stdin",
		"the child's stdin must be the reader Suspend lends (so gcloud detects an interactive TTY)")
}

func TestGcloudStreaming_ReturnsErrorOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX sh")
	}
	old := gcloudBin
	t.Cleanup(func() { gcloudBin = old })
	gcloudBin = "sh"

	rep := &stubReporter{interactive: true}
	err := GcloudStreaming(context.Background(), rep, "-c", "exit 3")
	require.Error(t, err)
}
