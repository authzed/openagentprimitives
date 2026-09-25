package installcmd

import (
	"io"
	"os"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// clioutReporter adapts cmd/oap's cliout package to the cloud.Reporter interface.
// It routes Step/OK/Info/Warn to the corresponding cliout helpers, and reports
// interactivity by checking whether stdin is a terminal (delegating to
// apcmd.StdinIsInteractive defined in agent_chat.go).
type clioutReporter struct {
	out io.Writer
}

func (r clioutReporter) Step(format string, args ...any) { cliout.Step(r.out, format, args...) }
func (r clioutReporter) OK(format string, args ...any)   { cliout.OK(r.out, format, args...) }
func (r clioutReporter) Info(format string, args ...any) { cliout.Info(r.out, format, args...) }
func (r clioutReporter) Warn(format string, args ...any) { cliout.Warn(r.out, format, args...) }
func (r clioutReporter) Interactive() bool               { return apcmd.StdinIsInteractive(os.Stdin) }

// Suspend has no live region to quiesce (this reporter streams plainly), so it
// just hands fn the writer and the process stdin (so a streamed subprocess can
// inherit the real terminal for TTY detection).
func (r clioutReporter) Suspend(fn func(out io.Writer, in io.Reader)) { fn(r.out, os.Stdin) }

// newCloudReporter returns a cloud.Reporter that writes to out using cliout
// helpers and reads stdin to decide interactivity. Kept for the non-rep callers
// (e.g. the dry-run paths) that don't have a progress.Reporter in hand.
func newCloudReporter(out io.Writer) cloud.Reporter {
	return clioutReporter{out: out}
}

// repReporter adapts the install progress.Reporter to the cloud.Reporter
// interface, so the cloud/gateway install flow narrates through the same
// checklist UI as the data plane instead of raw streaming output. progress has
// no Step concept — a cloud "Step" is just progress narration, so it routes to
// rep.Info (NOT a Phase row, which would leave a dangling checklist row).
// Interactivity still reflects whether stdin is a terminal.
type repReporter struct {
	rep progress.Reporter
}

func (r repReporter) Step(format string, args ...any) { r.rep.Info(format, args...) }
func (r repReporter) OK(format string, args ...any)   { r.rep.OK(format, args...) }
func (r repReporter) Info(format string, args ...any) { r.rep.Info(format, args...) }
func (r repReporter) Warn(format string, args ...any) { r.rep.Warn(format, args...) }
func (r repReporter) Interactive() bool               { return apcmd.StdinIsInteractive(os.Stdin) }

// Suspend delegates to the progress reporter, which quiesces and clears the live
// checklist region while fn owns the terminal (a prompt or a streamed gcloud).
func (r repReporter) Suspend(fn func(out io.Writer, in io.Reader)) { r.rep.Suspend(fn) }

// newCloudReporterRep returns a cloud.Reporter that narrates through the install
// progress reporter (the checklist UI), keeping the cloud/gateway flow visually
// consistent with the data plane.
func newCloudReporterRep(rep progress.Reporter) cloud.Reporter {
	return repReporter{rep: rep}
}

// cloudClients builds a cloud.Clients from a kube.Bundle.
func cloudClients(b *kube.Bundle) cloud.Clients {
	return cloud.Clients{
		REST:    b.REST,
		Typed:   b.Typed,
		Dynamic: b.Dynamic,
		Ctrl:    b.Controller,
	}
}
