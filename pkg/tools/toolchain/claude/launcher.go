// Package claude holds launcher-side logic specific to the "claude" toolchain
// (Claude Code, the inner coding agent running inside the sandbox). It is
// deliberately separate from pkg/tools/toolchain/kinds and
// pkg/tools/toolchain/resolve: those packages know how to deliver a
// toolchain's payload into the pod, not how any one payload's own binary
// discovers its configuration once it is running. The layout of ~/.claude is
// Claude Code knowledge, not podspec knowledge — putting it in the generic
// pod builder would make a future codex or gemini toolchain a branch there.
package claude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LinkStagedSkills makes AP-staged skill bundles visible to Claude Code.
//
// AP stages bundles read-only at /skills/<name>/ (see
// pkg/platform/podspec.applyMounts); Claude Code looks for user-level
// skills at $HOME/.claude/skills/. Symlinking bridges the two without copying
// (the bundles can be large and /tmp is memory-backed and charged against the
// pod). /skills itself MUST stay mounted read-only in the sandbox container —
// this function only ever reads from it and writes into $HOME, never back
// into skillsDir.
//
// Absent source dir is not an error: a session with no opted-in skills has no
// /skills mount at all, and the launcher must still exec claude rather than
// failing the tool call.
func LinkStagedSkills(skillsDir, home string) error {
	entries, err := os.ReadDir(skillsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read staged skills at %s: %w", skillsDir, err)
	}
	dest := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		link := filepath.Join(dest, e.Name())
		// Idempotent: the launcher runs on every claude invocation.
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replace stale link %s: %w", link, err)
		}
		if err := os.Symlink(filepath.Join(skillsDir, e.Name()), link); err != nil {
			return fmt.Errorf("link skill %s: %w", e.Name(), err)
		}
	}
	return nil
}
