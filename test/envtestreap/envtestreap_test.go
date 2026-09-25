package envtestreap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real `ps -ax -o pid,ppid,command` output shape: a header line, right-aligned
// numeric columns, and a command path that contains spaces ("Application
// Support"), which is exactly what a naive field split gets wrong.
const psSample = `  PID  PPID COMMAND
30894     1 /Users/dev/Library/Application Support/io.kubebuilder.envtest/k8s/1.30.3-darwin-arm64/etcd --advertise-client-urls=http://127.0.0.1:60731
30896     1 /Users/dev/Library/Application Support/io.kubebuilder.envtest/k8s/1.30.3-darwin-arm64/kube-apiserver --cert-dir=/tmp/k8s
44100 44099 /Users/dev/Library/Application Support/io.kubebuilder.envtest/k8s/1.30.3-darwin-arm64/etcd --listen-peer-urls=http://localhost:0
  512     1 /usr/libexec/secinitd
 9412     1 /Applications/Docker.app/Contents/MacOS/com.docker.backend
`

func TestOrphans_SelectsOnlyReparentedEnvtestProcesses(t *testing.T) {
	got := Orphans(psSample)
	assert.Equal(t, []int{30894, 30896}, got,
		"only envtest binaries whose parent already died (PPID 1) are orphans")
}

// A live run's apiserver still has its test binary as parent. Killing those
// would murder a suite that is currently passing — the one outcome a reaper
// must never produce.
func TestOrphans_SparesEnvtestProcessesWithALivingParent(t *testing.T) {
	assert.NotContains(t, Orphans(psSample), 44100,
		"PPID 44099 is a running test binary; its apiserver is in use")
}

// PID 1 owns plenty of unrelated daemons. The envtest asset path is the only
// thing that makes a process ours to kill.
func TestOrphans_IgnoresUnrelatedInitOwnedProcesses(t *testing.T) {
	got := Orphans(psSample)
	assert.NotContains(t, got, 512)
	assert.NotContains(t, got, 9412)
}

func TestOrphans_EmptyAndHeaderOnlyInputAreSafe(t *testing.T) {
	assert.Empty(t, Orphans(""))
	assert.Empty(t, Orphans("  PID  PPID COMMAND\n"))
}

// A malformed row must not take down the sweep — the reaper runs before every
// suite, so it has to be impossible for it to panic the build.
func TestOrphans_SkipsMalformedRowsWithoutPanicking(t *testing.T) {
	junk := "  PID  PPID COMMAND\ngarbage\nx y io.kubebuilder.envtest/etcd\n77 1 /a/io.kubebuilder.envtest/k8s/etcd\n"
	var got []int
	require.NotPanics(t, func() { got = Orphans(junk) })
	assert.Equal(t, []int{77}, got)
}
