// Package menubaricons defines the oap desktop menu-bar status icon states and
// the animator that drives them. The icon art is a solid triangle rendered as
// a macOS template image; the render subpackage (build-time) rasterizes the
// frames and the assets subpackage embeds them.
package menubaricons

// State is one oap desktop lifecycle state shown in the menu bar.
type State int

const (
	StateSetup        State = iota // bringing the cluster up (animated)
	StateRunning                   // up and reachable (static)
	StateShuttingDown              // stopping the VM (animated)
	StateStopped                   // stopped but installed (static)
	StateUninstalling              // tearing down (animated)
	StateError                     // faulted (static)
	StateQuitting                  // VM stopped, app closing (animated)
)

// animatedFrames is the per-loop frame count for animated states. One loop
// plays over animatedFrames * Animator interval.
const animatedFrames = 12

// AllStates lists every state in declaration order; the generator and golden
// test iterate it. Keep it exhaustive.
func AllStates() []State {
	return []State{StateSetup, StateRunning, StateShuttingDown, StateStopped, StateUninstalling, StateError, StateQuitting}
}

// Basename is the asset filename stem: frames are "<basename>_<NN>.png".
func (s State) Basename() string {
	switch s {
	case StateSetup:
		return "setup"
	case StateRunning:
		return "running"
	case StateShuttingDown:
		return "shuttingdown"
	case StateStopped:
		return "stopped"
	case StateUninstalling:
		return "uninstalling"
	case StateError:
		return "error"
	case StateQuitting:
		return "quitting"
	default:
		return "unknown"
	}
}

// Animated reports whether the state loops (in-progress) vs. holds one still
// frame (steady).
func (s State) Animated() bool {
	switch s {
	case StateSetup, StateShuttingDown, StateUninstalling, StateQuitting:
		return true
	default:
		return false
	}
}

// FrameCount is how many animation frames a state has.
func (s State) FrameCount() int {
	if s.Animated() {
		return animatedFrames
	}
	return 1
}
