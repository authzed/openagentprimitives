package installcmd

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// workspaceOption is one entry in the interactive workspace-storage picker: a
// concrete RWX class, or the isolated /work choice (className empty).
type workspaceOption struct {
	className string
	isolated  bool
}

// classBackendDesc is the human-readable backend description shown after each
// class name, so an operator can tell a node-pinned local class from a
// cross-node Filestore one at a glance — the whole point of the picker.
func classBackendDesc(c cloud.RWXClassInfo) string {
	switch {
	case c.Bundled:
		return "local provisioner, single-node (node-pinned)"
	case c.Filestore && c.Multishare:
		return "Filestore, cross-node, shared instance"
	case c.Filestore:
		return "Filestore, cross-node, dedicated ≥1 TiB per session"
	default:
		return "cross-node RWX"
	}
}

// workspaceClassLabel renders one menu line: the class name, any (current /
// recommended) tags, and its backend description.
func workspaceClassLabel(c cloud.RWXClassInfo, current, recommended string) string {
	var tags []string
	if c.Name == current {
		tags = append(tags, "current")
	}
	if c.Name == recommended {
		tags = append(tags, "recommended")
	}
	label := c.Name
	if len(tags) > 0 {
		label += "  (" + strings.Join(tags, ", ") + ")"
	}
	return label + " — " + classBackendDesc(c)
}

// workspaceClassOfferOptions builds the picker menu from the cluster's RWX
// classes. The current class is pre-selected (the returned 1-based defaultIdx)
// and the recommended class is annotated. The isolated /work choice is always
// last. If the current (marker) class no longer exists on the cluster, it is
// still listed first and kept as the default so the menu defaults sanely and
// the user can switch away from it. Pure — no cluster access, table-testable.
func workspaceClassOfferOptions(classes []cloud.RWXClassInfo, current, recommended string) (labels []string, options []workspaceOption, defaultIdx int) {
	currentSeen := false
	for _, c := range classes {
		if c.Name == current {
			currentSeen = true
		}
		labels = append(labels, workspaceClassLabel(c, current, recommended))
		options = append(options, workspaceOption{className: c.Name})
	}
	if !currentSeen && current != "" {
		labels = append([]string{fmt.Sprintf("%s  (current — not found on cluster)", current)}, labels...)
		options = append([]workspaceOption{{className: current}}, options...)
	}
	labels = append(labels, "isolated /work (no shared workspace — multi-bundle agents lose shared state)")
	options = append(options, workspaceOption{isolated: true})

	defaultIdx = 1
	for i, o := range options {
		if !o.isolated && o.className == current {
			defaultIdx = i + 1
			break
		}
	}
	return labels, options, defaultIdx
}

// workspaceClassIsBillable reports whether the named class is a billable managed
// RWX backend (a GKE Filestore CSI class) — the classes the linear install gates
// behind a cost-confirm. Derived from cloud.ListRWXClasses' per-class
// classification (the same RWXClassInfo.Filestore the picker labels from), not a
// provisioner string re-derived here. A name absent from classes (an unknown
// custom class) is treated as not-billable: nothing has told us it costs.
func workspaceClassIsBillable(name string, classes []cloud.RWXClassInfo) bool {
	for _, c := range classes {
		if c.Name == name {
			return c.Filestore
		}
	}
	return false
}

// wizardWorkspaceExplicitClass folds the Workspace screen's answer into the
// ExplicitClass the wizard forwards to RunInstall. Keeping the detected class
// (or an empty pick) forwards NOTHING, so RunInstall reads the marker and uses
// it directly (no probe). A genuine change forwards the class so RunInstall
// probes it — but only when:
//
//   - the run is interactive. A non-interactive driver fabricates the screen's
//     default on EOF (pkg/cli/tui/driver.go), and a fabricated pick must never
//     become a billable explicit override (the "never silently enable billable
//     Filestore" contract). A non-TTY run falls through with "" so RunInstall's
//     own non-interactive handling decides safely.
//   - a billable managed (Filestore) class additionally clears the cost
//     consent the screen never surfaced. A decline forwards "" and RunInstall
//     resolves normally (bundled/isolated), never the billable class.
func wizardWorkspaceExplicitClass(chosen, detected string, classes []cloud.RWXClassInfo, interactive bool, confirmBillable func(class string) bool) string {
	if chosen == "" || chosen == detected {
		return ""
	}
	if !interactive {
		return ""
	}
	if workspaceClassIsBillable(chosen, classes) && !confirmBillable(chosen) {
		return ""
	}
	return chosen
}

// selectWorkspaceClass renders the interactive workspace-storage picker (current
// pre-selected, recommendation annotated), warns that switching rolls the
// operator, reads one line from in, and maps the choice to a workspaceChoice.
// Empty, unrecognized, or out-of-range input keeps the current class. A concrete
// class is returned as ClassName (repointing is the caller's job); the isolated
// choice returns an empty ClassName with guidance for the WORK block.
func selectWorkspaceClass(in io.Reader, out io.Writer, classes []cloud.RWXClassInfo, current, recommended string) workspaceChoice {
	labels, options, defaultIdx := workspaceClassOfferOptions(classes, current, recommended)

	cliout.Step(out, "workspace storage (shared RWX)")
	cliout.Warn(out, "changing this rolls the operator (spicebox-operator restarts), interrupting any in-flight sessions.")
	for i, label := range labels {
		fmt.Fprintf(out, "  %d) %s\n", i+1, label)
	}
	cliout.Prompt(out, "Workspace storage class? [1-%d, default=%d]: ", len(labels), defaultIdx)

	choice := defaultIdx
	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		if s := strings.TrimSpace(scanner.Text()); s != "" {
			var idx int
			if n, _ := fmt.Sscan(s, &idx); n == 1 && idx >= 1 && idx <= len(labels) {
				choice = idx
			}
			// Unrecognized / out-of-range input keeps the default (current).
		}
	}

	opt := options[choice-1]
	if opt.isolated {
		return workspaceChoice{Message: "workspace storage: isolated /work (no shared workspace); multi-bundle agents use pod-local /work only"}
	}
	// Keeping the current (marker) class is trusted directly — a prior install
	// already probed it and stamped the operator, so no re-probe. SWITCHING to a
	// different class is unverified: it must be probed before the WORK block
	// writes the marker (which every future install then trusts) or stamps it
	// onto the serving operator. Without Probe here a picked-but-broken class —
	// a disabled Filestore API, an unsatisfiable size floor — would be recorded
	// as "already verified" and silently reinstated on every later run.
	return workspaceChoice{ClassName: opt.className, Probe: opt.className != current}
}
