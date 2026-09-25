// What a wizard run leaves behind: the manifests it produced, applied to the
// cluster, and the record of the run that stays in the operator's scrollback.
//
// WHICH objects go, in what order, under which field manager, and what is
// refused in front of them is wizardrun.Apply's — because admind lands the
// same WizardOutput and cannot import this package. What is HERE is the one
// thing the two clients genuinely do differently: `oap` writes through a
// dynamic client that resolves the GVR from the embedded CRDs, and a
// server-side caller writes through its own controller-runtime client.
//
// What is NOT here is how a run is presented: the theme, the streams and the
// chrome arrive from whichever command built them, the same way they do for
// Run.
package channelwizard

import (
	"context"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// dynApplier writes one object through oap's dynamic client.
//
// It is the CLI half of wizardrun.Applier, and the only thing in this file
// that is genuinely oap's rather than every client's. kube.Apply resolves the
// GVR from the embedded CRDs and stamps
// metadata.annotations["agentprimitives.authzed.com/installed-by"]=ap onto
// whatever it is handed — including the AgentClass the capability patch
// touches. That is accepted: the annotation is what `oap clean` reads to tell
// ap-managed resources from pre-existing cluster infrastructure, and an
// AgentClass oap is binding a channel to is ap-managed. It is stable across
// re-runs, so it does not break the byte-identical-re-apply requirement.
type dynApplier struct{ dyn dynamic.Interface }

func (a dynApplier) ApplyObject(ctx context.Context, obj *unstructured.Unstructured, fieldManager string) error {
	return kube.Apply(ctx, a.dyn, obj, fieldManager)
}

// Apply server-side-applies what a channel wizard produced: the credentials
// Secret, the Channel, and the capability patch for the AgentClass the Channel
// binds to.
//
// The decisions — the refuse-to-overwrite net, the object order, the two field
// managers, the capability patch going last — are wizardrun.Apply's, shared
// with admind. This adapts oap's client bundle to it.
func Apply(ctx context.Context, b *kube.Bundle, wizOut channelkinds.WizardOutput) error {
	return wizardrun.Apply(ctx, b.Controller, dynApplier{dyn: b.Dynamic}, wizOut)
}

// RunNotes is what a run's post-run summary shows: what THIS PACKAGE recorded
// as the run went (tui.Question's own notes, and what runHandoff recorded
// about the browser round trip), then what the kind's Result declared.
//
// Both halves exist because a kind runs no code of its own while the
// questions are being answered: anything it decided has to arrive as data on
// its WizardOutput, while anything the dispatcher decided is already in
// State. Rendering both through one function is what puts them in one block
// — and what makes a line recorded in both places render twice, which is why
// WizardOutput.Summary says to state it in one.
func RunNotes(answered *tui.State, wizOut channelkinds.WizardOutput) []tui.Note {
	var notes []tui.Note
	if answered != nil {
		notes = answered.Notes()
	}
	for _, n := range wizOut.Summary {
		notes = append(notes, tui.Note{Label: n.Label, Value: n.Value})
	}
	return notes
}

// ReportUnfinished renders what a failed run had already DONE before it
// returns the reason it stopped.
//
// The notes are not decoration: some of them record work that outlived the
// run and that nothing else can recover. The Slack kind's provisioning route
// records the app it created and installed, and no Slack API lists a user's
// apps — so a failure between that step and the end of the run, with the
// summary rendered only on the success path, would leave a real app in the
// user's workspace whose ID had been printed nowhere. What the run created
// and why it stopped are both facts; a failure suppresses neither.
//
// The cause is returned unchanged, so the caller still fails, and a summary
// that cannot be written is joined onto it rather than dropped.
//
// notes is the SAME list the success path renders (RunNotes), not just what
// the run's own steps recorded: a kind states its decisions as data on its
// WizardOutput, so that is the only place they exist, and a failure after the
// wizard would otherwise print nothing at all.
func ReportUnfinished(out io.Writer, theme *tui.Theme, notes []tui.Note, cause error) error {
	if len(notes) == 0 {
		return cause
	}
	if err := tui.RenderSummary(out, theme, notes); err != nil {
		return errors.Join(cause, fmt.Errorf("render what the failed run had already done: %w", err))
	}
	return cause
}

// RenderNextSteps prints the kind's post-setup guidance: the facts about a
// channel that now exists, which belong after the run rather than as a wall of
// prose in front of the first question.
//
// Rendered as its own block rather than through tui.RenderSummary because
// these are sentences, not decisions: RenderSummary pads labels into a column
// to make short label/value pairs scannable, which a paragraph would blow out.
func RenderNextSteps(w io.Writer, th *tui.Theme, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", th.Render(th.Title, "Next steps"))
	for _, n := range notes {
		fmt.Fprintf(w, "  %s %s\n", th.Render(th.Subtle, "•"), n)
	}
}
