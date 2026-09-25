//go:build darwin && arm64

package desktopcmd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/setupui"
)

// TestBringUpStepNamesInitializingFirst pins the ordering invariant the
// synthetic "Initializing" step relies on: it must be first, ahead of the
// first real Engine.Up step, so that marking it active up front (bringUp) and
// having the first Up step auto-complete it (Timeline.Begin marks every earlier
// step done) both work. If someone reorders bringUpStepNames such that
// "Initializing" is no longer first, this fails loudly instead of the timeline
// silently regressing to a wall of pending rows.
func TestBringUpStepNamesInitializingFirst(t *testing.T) {
	names := bringUpStepNames(desktop.Config{})
	require.NotEmpty(t, names)
	require.Equal(t, "Initializing", names[0], `"Initializing" must be the first seeded step`)
	require.Equal(t, "Provisioning disk image", names[1], "first real Engine.Up step must follow Initializing")

	// End-to-end contract: bringUp marks "Initializing" active up front, and the
	// first real step then auto-completes it — mirroring what the setup window
	// observes over /events.
	tl := setupui.NewTimeline(names)
	tl.Begin("Initializing", 1000)
	snap := tl.Snapshot()
	assert.Equal(t, setupui.PhaseRunning, snap.Phase, "Begin flips the timeline out of configuring")
	assert.Equal(t, setupui.StatusActive, snap.Steps[0].Status, "Initializing is active immediately")

	tl.Begin("Provisioning disk image", 2000)
	snap = tl.Snapshot()
	assert.Equal(t, setupui.StatusDone, snap.Steps[0].Status, "first real step auto-completes Initializing")
	assert.Equal(t, setupui.StatusActive, snap.Steps[1].Status, "first real step is now active")
}

// statFileWithTimes writes content to a fresh file under t.TempDir() and
// pins its mtime via os.Chtimes, so tests can construct os.FileInfo values
// with exact, independently-controlled sizes and mtimes without depending
// on filesystem timing.
func statFileWithTimes(t *testing.T, name string, content []byte, mtime time.Time) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi
}

func TestShouldRestageDiskImage(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bundled := statFileWithTimes(t, "bundled.img", []byte("aaaa"), base)
	id := bundledRootfsIdentity(bundled)

	t.Run("no staged copy yet -> restage", func(t *testing.T) {
		assert.True(t, shouldRestageDiskImage(false, "", bundled))
	})

	t.Run("marker matches the bundled identity -> reuse", func(t *testing.T) {
		assert.False(t, shouldRestageDiskImage(true, id, bundled))
	})

	t.Run("marker differs (rebuilt .app shipped a new rootfs) -> restage", func(t *testing.T) {
		assert.True(t, shouldRestageDiskImage(true, "size=1 mtime=1", bundled))
	})

	t.Run("empty marker (legacy staged copy predating the marker) -> restage once", func(t *testing.T) {
		assert.True(t, shouldRestageDiskImage(true, "", bundled))
	})

	t.Run("marker matches regardless of staged-disk mtime drift -> reuse (the live-VM-disk fix)", func(t *testing.T) {
		// The decision no longer consults the staged disk's own stats, so a
		// running VM whose writable-disk mtime has advanced far past the
		// bundled rootfs still reuses, as long as the recorded marker matches.
		assert.False(t, shouldRestageDiskImage(true, id, bundled))
	})

	t.Run("bundled missing entirely -> reuse staged (caller's os.Open surfaces the real error)", func(t *testing.T) {
		assert.False(t, shouldRestageDiskImage(true, "any-marker", nil))
	})
}

func TestEnsureDiskImage(t *testing.T) {
	t.Run("first run: no staged copy -> copies from resources + records marker", func(t *testing.T) {
		resourcesDir := t.TempDir()
		supportDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(resourcesDir, rootfsImageName), []byte("bundled-v1"), 0o644))

		dst, err := ensureDiskImage(resourcesDir, supportDir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(supportDir, rootfsImageName), dst)
		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		assert.Equal(t, "bundled-v1", string(got))
		_, err = os.Stat(filepath.Join(supportDir, rootfsStagedFromMarker))
		require.NoError(t, err, "the stage must record a rootfs identity marker")
	})

	t.Run("relaunch, unchanged bundle, live VM disk mutated -> reused (VM state preserved)", func(t *testing.T) {
		resourcesDir := t.TempDir()
		supportDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(resourcesDir, rootfsImageName), []byte("bundled-v1"), 0o644))
		// First launch: stage + record the marker.
		_, err := ensureDiskImage(resourcesDir, supportDir)
		require.NoError(t, err)
		// Simulate the running VM writing to its disk: content changes and the
		// mtime jumps far past the bundled rootfs — exactly what defeated the
		// old staged-vs-bundled mtime comparison.
		staged := filepath.Join(supportDir, rootfsImageName)
		require.NoError(t, os.WriteFile(staged, []byte("LIVE-VM-STATE"), 0o600))
		future := time.Now().Add(48 * time.Hour)
		require.NoError(t, os.Chtimes(staged, future, future))

		// Second launch: bundle unchanged -> reuse, preserving VM disk state.
		_, err = ensureDiskImage(resourcesDir, supportDir)
		require.NoError(t, err)
		got, err := os.ReadFile(staged)
		require.NoError(t, err)
		assert.Equal(t, "LIVE-VM-STATE", string(got), "an unchanged bundle must reuse the live VM disk, not overwrite it")
	})

	t.Run("rebuilt bundle ships a different rootfs -> re-staged despite newer staged-disk mtime", func(t *testing.T) {
		resourcesDir := t.TempDir()
		supportDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(resourcesDir, rootfsImageName), []byte("bundled-v1"), 0o644))
		_, err := ensureDiskImage(resourcesDir, supportDir)
		require.NoError(t, err)
		// Live VM disk advances its mtime far into the future (which defeated
		// the old mtime check), then a rebuilt .app ships a different rootfs.
		staged := filepath.Join(supportDir, rootfsImageName)
		future := time.Now().Add(48 * time.Hour)
		require.NoError(t, os.Chtimes(staged, future, future))
		require.NoError(t, os.WriteFile(filepath.Join(resourcesDir, rootfsImageName), []byte("bundled-v2-different"), 0o644))

		_, err = ensureDiskImage(resourcesDir, supportDir)
		require.NoError(t, err)
		got, err := os.ReadFile(staged)
		require.NoError(t, err)
		assert.Equal(t, "bundled-v2-different", string(got), "a rebuilt bundle must re-stage even when the staged disk's mtime is newer")
	})

	t.Run("bundled image missing -> actionable error", func(t *testing.T) {
		resourcesDir := t.TempDir()
		supportDir := t.TempDir()

		_, err := ensureDiskImage(resourcesDir, supportDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mage desktop:app")
	})
}

func TestEnsureVMKey(t *testing.T) {
	t.Run("fresh stage: copies key to support dir at 0600", func(t *testing.T) {
		resourcesDir := t.TempDir()
		supportDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(resourcesDir, sshKeyResourceName), []byte("PRIVATE-KEY-v1"), 0o600))

		require.NoError(t, ensureVMKey(resourcesDir, supportDir))

		dst := filepath.Join(supportDir, sshKeyResourceName)
		got, err := os.ReadFile(dst)
		require.NoError(t, err, "staged key must exist")
		assert.Equal(t, []byte("PRIVATE-KEY-v1"), got)
		fi, err := os.Stat(dst)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "staged key must be 0600")
	})

	t.Run("idempotent: unchanged source is a no-op, still staged", func(t *testing.T) {
		resourcesDir := t.TempDir()
		supportDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(resourcesDir, sshKeyResourceName), []byte("PRIVATE-KEY-v1"), 0o600))
		require.NoError(t, ensureVMKey(resourcesDir, supportDir))
		require.NoError(t, ensureVMKey(resourcesDir, supportDir), "second call must not error")

		got, err := os.ReadFile(filepath.Join(supportDir, sshKeyResourceName))
		require.NoError(t, err)
		assert.Equal(t, []byte("PRIVATE-KEY-v1"), got)
	})

	t.Run("rotation: changed source overwrites staged copy", func(t *testing.T) {
		resourcesDir := t.TempDir()
		supportDir := t.TempDir()
		keySrc := filepath.Join(resourcesDir, sshKeyResourceName)
		require.NoError(t, os.WriteFile(keySrc, []byte("PRIVATE-KEY-v1"), 0o600))
		require.NoError(t, ensureVMKey(resourcesDir, supportDir))

		require.NoError(t, os.WriteFile(keySrc, []byte("PRIVATE-KEY-v2-rotated"), 0o600))
		require.NoError(t, ensureVMKey(resourcesDir, supportDir))

		got, err := os.ReadFile(filepath.Join(supportDir, sshKeyResourceName))
		require.NoError(t, err)
		assert.Equal(t, []byte("PRIVATE-KEY-v2-rotated"), got)

		fi, err := os.Stat(filepath.Join(supportDir, sshKeyResourceName))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "rotated key must remain 0600")
	})

	t.Run("missing source: returns an error", func(t *testing.T) {
		resourcesDir := t.TempDir() // no ap-vm-key inside
		supportDir := t.TempDir()
		assert.Error(t, ensureVMKey(resourcesDir, supportDir))
	})
}

// TestInstallHookSelectsTheDesktopCloudKind guards installHook's
// installcmd.RunInstall wiring: it must pass cloud.MustFor(cloud.KeyDesktop),
// never cloud.MustFor(cloud.KeyLocal). The desktop VM and --local both use the
// lightweight dev profile, but only desktop's InstallProfile serves the
// built-in web chat (see pkg/platform/cloud/desktop) — that VM is network-confined and
// single-user, unlike --local's public ngrok tunnel, where the chat's
// single-user-by-design posture would otherwise be reachable from the public
// internet. installHook isn't otherwise unit-testable (it drives a real VM
// bring-up), so this reads the source, mirroring
// internal/cmd/webd/sharedorigin_wiring_test.go's approach for the same class of bug.
//
// It scans every non-test file in the package rather than naming one: a guard
// pinned to a single filename stops guarding the moment the code it watches
// moves next door.
func TestInstallHookSelectsTheDesktopCloudKind(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "read the package directory")

	var src string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		require.NoError(t, err, "read %s", name)
		src += string(b)
	}
	require.NotEmpty(t, src, "the scan must have found the package's sources")

	require.Regexp(t,
		regexp.MustCompile(`cloud\.MustFor\(cloud\.KeyDesktop\)`),
		src, "installHook must pass cloud.MustFor(cloud.KeyDesktop) into installcmd.RunInstall")

	assert.NotContains(t, src, "cloud.MustFor(cloud.KeyLocal)",
		"regression guard: oap desktop must never install via cloud.KeyLocal again — "+
			"that is --local's public-tunnel kind, whose InstallProfile serves NO built-in web chat")
}

// TestDoneFired covers the liveness check ensureWebdPortForward gates its
// cached port-forward on.
//
// The cache without this check was a durability defect, not a cosmetic one: a
// port-forward binds to ONE pod, so any webd rollout or eviction kills it, and
// the previous code kept handing the dead forwarder back forever. The menu
// then opened a browser at a port nothing was listening on, with a healthy
// cluster and a healthy webd behind it, and nothing short of restarting the
// app could recover — in-memory state whose invalidity was silent and
// unrecoverable.
func TestDoneFired(t *testing.T) {
	t.Run("a stream that ended with an error is dead", func(t *testing.T) {
		// The real shape: portforward's done channel is buffered(1) and
		// receives ForwardPorts' return value when the stream dies.
		done := make(chan error, 1)
		done <- errors.New("lost connection to pod")
		assert.True(t, doneFired(done))
	})

	t.Run("a stream that ended CLEANLY is dead too", func(t *testing.T) {
		// nil means a clean shutdown (Stop/ctx-cancel), which is still gone.
		// Reading only the error's non-nil-ness would call this one alive and
		// keep serving a forwarder that stopped on purpose.
		done := make(chan error, 1)
		done <- nil
		assert.True(t, doneFired(done))
	})

	t.Run("a closed channel is dead", func(t *testing.T) {
		done := make(chan error, 1)
		close(done)
		assert.True(t, doneFired(done))
	})

	t.Run("a live stream is NOT dead, and the check does not block", func(t *testing.T) {
		// The click path runs this on every menu press, so a healthy forwarder
		// must cost nothing. A blocking read here would hang the UI thread
		// forever instead of opening the browser.
		done := make(chan error, 1)
		fired := make(chan bool, 1)
		go func() { fired <- doneFired(done) }()
		select {
		case got := <-fired:
			assert.False(t, got)
		case <-time.After(2 * time.Second):
			t.Fatal("doneFired blocked on a live forwarder; it must be a non-blocking check")
		}
	})

	t.Run("a forwarder that never started reads as not-dead", func(t *testing.T) {
		// portforward.New leaves done nil until Start succeeds. Such a
		// forwarder is never cached, and a nil channel is the one input where
		// a blocking select would hang forever — so it must be handled
		// explicitly rather than falling through to the receive.
		assert.False(t, doneFired(nil))
	})
}
