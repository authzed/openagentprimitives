//go:build darwin && arm64

package desktopcmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// instanceLockName is the lock file, under the Application Support dir, that
// makes `oap desktop` single-instance.
const instanceLockName = "desktop.lock"

// errAlreadyRunning means another `oap desktop` process holds the instance lock.
var errAlreadyRunning = errors.New("another OAP Desktop instance is already running")

// alreadyRunningMessage is the notification a second instance shows before
// exiting. The usual cause is `oap desktop` running from a terminal when an
// install link makes LaunchServices start the .app.
const alreadyRunningMessage = "OAP Desktop is already running (perhaps from a terminal). Install links only reach the OAP Desktop app — quit the other copy and try again."

// acquireInstanceLock takes an exclusive, non-blocking flock on
// <supportDir>/desktop.lock. The caller must keep the returned file open for
// the life of the process: closing it (or exiting) releases the lock. A lock
// already held — by another process, or by another open file in this one —
// yields an error wrapping errAlreadyRunning.
//
// It must run before anything that assumes this is the only instance, above
// all the startup orphan-VM cleanup, which would otherwise kill the other
// instance's live VM.
func acquireInstanceLock(supportDir string) (*os.File, error) {
	path := filepath.Join(supportDir, instanceLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock %s held)", errAlreadyRunning, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}
