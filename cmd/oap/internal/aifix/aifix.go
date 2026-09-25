// Package aifix is the pluggable "AI fixer" seam for a stalled `oap install`
// wait. When an install readiness wait stalls and `oap` has surfaced diagnostics
// (the blocking pod's phase + Warning events + PVC state), the keep-waiting
// prompt offers an extra key that launches whichever LLM CLI the user already
// has installed — claude / codex / gemini — pre-loaded with a prompt pointing at
// the repo root and the gathered context, to diagnose and fix the failure.
//
// Safety posture: the fix is USER-INITIATED. `oap` only assembles context and
// execs the user's own installed CLI in their own shell with their own
// credentials; the launched agent's permission model governs whether it applies
// any change. `oap` never edits files or the cluster itself.
//
// Pluggability (registry-style per the repo's "pluggability is a primary design
// goal"): each launcher self-registers via init(); FirstAvailable returns the
// first one detected on PATH. A new backend is registered, not branched on.
package aifix

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

// Launcher launches a user-installed AI CLI, pre-loaded with a diagnostic
// prompt, to diagnose and fix a stalled install wait.
type Launcher interface {
	// Name is the CLI's command name, e.g. "claude", "codex", "gemini".
	Name() string
	// Detect probes $PATH for the CLI; ok is false when it is not installed.
	Detect() (path string, ok bool)
	// Launch execs the CLI interactively (stdin/stdout/stderr wired to the
	// user's terminal), passing the assembled prompt. It returns an error if
	// the launch itself failed; a non-zero exit from the CLI is also surfaced.
	Launch(ctx context.Context, prompt string) error
}

// registry holds every registered Launcher in registration order. FirstAvailable
// walks it in order, so the registration sequence is the preference order.
var registry []Launcher

// Register adds a Launcher to the registry. Called from each launcher's init().
func Register(l Launcher) { registry = append(registry, l) }

// Registered returns the registered launchers in registration (preference)
// order. The returned slice is a copy, safe for the caller to retain.
func Registered() []Launcher {
	out := make([]Launcher, len(registry))
	copy(out, registry)
	return out
}

// FirstAvailable returns the first registered Launcher whose CLI is detected on
// $PATH, or (nil, false) when none is installed.
func FirstAvailable() (Launcher, bool) {
	for _, l := range registry {
		if _, ok := l.Detect(); ok {
			return l, true
		}
	}
	return nil, false
}

// lookPath is the PATH probe used by every launcher's Detect. It is a package
// var so tests can stub CLI presence without a real binary on PATH.
var lookPath = exec.LookPath

// BuildPrompt assembles the prompt handed to the launched CLI. It includes the
// repo root path (so the agent can read the install code), the failing
// component, the cluster context, and the gathered diagnostics (pod phase +
// Warning events + PVC state), then instructions to unblock the install with a
// runtime-only fix (no code edits) and to write all of its findings to a dated,
// problem-named file that a follow-up agent can turn into code improvements.
func BuildPrompt(repoRoot, component string, diag wait.Diagnosis, kubeContext, cloud string) string {
	var b strings.Builder
	b.WriteString("An `oap install` readiness wait has stalled and needs help.\n\n")
	fmt.Fprintf(&b, "Repository root: %s\n", repoRoot)
	if kubeContext != "" {
		fmt.Fprintf(&b, "Kube-context: %s\n", kubeContext)
	}
	if cloud != "" {
		fmt.Fprintf(&b, "Cloud: %s\n", cloud)
	}
	fmt.Fprintf(&b, "Stalled component: %s\n", component)

	b.WriteString("\nDiagnostics gathered by ap:\n")
	wrote := false
	if diag.Pod != "" {
		fmt.Fprintf(&b, "  pod %s   %s", diag.Pod, diag.PodPhase)
		if diag.Reason != "" {
			fmt.Fprintf(&b, " / %s", diag.Reason)
		}
		b.WriteByte('\n')
		wrote = true
	}
	if t := diag.Terminated; t != nil {
		fmt.Fprintf(&b, "  container %s   exited %d / %s\n", t.Container, t.ExitCode, t.Reason)
		if t.Message != "" {
			fmt.Fprintf(&b, "    %s\n", t.Message)
		}
		for _, line := range t.Logs {
			fmt.Fprintf(&b, "    %s\n", line)
		}
		wrote = true
	}
	for _, pvc := range diag.PVCs {
		fmt.Fprintf(&b, "  PVC %s   %s\n", pvc.Name, pvc.Phase)
		wrote = true
	}
	for _, e := range diag.Events {
		if e.Count > 1 {
			fmt.Fprintf(&b, "  event (x%d): %s — %s\n", e.Count, e.Reason, e.Message)
		} else {
			fmt.Fprintf(&b, "  event: %s — %s\n", e.Reason, e.Message)
		}
		wrote = true
	}
	if !wrote {
		b.WriteString("  (no specific diagnostics were captured)\n")
	}

	fmt.Fprintf(&b, "\nYour goal is to get this `oap install` unblocked so %s becomes ready — "+
		"the user is waiting on their install to finish, not on a code change.\n\n", component)
	b.WriteString("Diagnose the failure: inspect the cluster with kubectl (and the cloud, if relevant) " +
		"and read the install code under the repository root to understand the root cause. " +
		"Then apply a fix at the cluster/runtime level only — kubectl edits, a cloud-CLI action, " +
		"reconfiguration, etc. — to unblock the install.\n\n")
	b.WriteString("Do NOT edit the install code or any files in the repository to fix this. " +
		"Treat the repo as read-only context. Runtime fixes only.\n\n")
	b.WriteString("Once the install is unblocked, write ALL of your findings and reasoning to a single " +
		"Markdown file in the repository root, named for the current date-time and a short slug of the " +
		"problem (for example, `2026-06-29-1430-postgres-pvc-stuck.md`; run `date` if you need the " +
		"current time). Capture everything: the symptom, the root cause, exactly what you changed at " +
		"runtime to unblock it, and concrete recommendations for code or install-flow improvements that " +
		"would prevent this failure from recurring. That file is the handoff to a follow-up agent that " +
		"makes the actual code changes, so be exhaustive — include every relevant detail and musing, not " +
		"just a summary.\n")
	return b.String()
}

// names returns the registered launcher names sorted, for error messages.
func names() string {
	ns := make([]string, 0, len(registry))
	for _, l := range registry {
		ns = append(ns, l.Name())
	}
	sort.Strings(ns)
	return strings.Join(ns, ", ")
}

// Names returns a human-readable, comma-separated list of registered launcher
// names (sorted), for "no AI CLI found (looked for …)" messages.
func Names() string { return names() }
