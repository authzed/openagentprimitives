// Package wizardrun is the client-neutral half of driving a channel kind's
// setup flow (channelkinds.Wizard): the ORDER of the steps once the answers
// are in, the collision check that guards them, and the apply that lands what
// they produced.
//
// It exists because there are now TWO clients and Go's own import rule keeps
// them apart. `oap channel create` and `oap agent install` drive the flow from
// a terminal, through cmd/oap/internal/channelwizard; admind's install form
// drives the same flow from an HTTP handler, and pkg/web/admind CANNOT import
// anything under cmd/oap/internal. Copying the sequence into the second client
// is the one outcome that must not happen: every step's position was bought
// with a bug (see Finish), and two copies would drift on exactly the steps
// whose order is not derivable from the contract.
//
// WHAT IS HERE IS WHAT BOTH CLIENTS SHARE, and no more. The front half of a
// run — which questions to render, how to render them, how to read an answer
// back — is irreducibly the client's: a terminal draws screens and a browser
// draws a form. So this package takes a COMPLETE answer map and starts where
// the two converge, and it takes the one interactive step no data can express
// (channelkinds.HandoffSpec) as a function the client supplies, because the
// client is what owns the callback: a loopback listener for the CLI, a route
// for the UI.
//
// It names no kind and no terminal type. A tui, huh, or bubbletea import
// appearing here would put this package back out of a server's reach, which is
// the one property it exists to have.
package wizardrun

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// RefuseExisting fails closed on a name that already names a Channel in the
// namespace. A nil client (an offline run, a preview with no cluster) or an
// empty name reports nothing to refuse.
//
// One function, four call sites — before the CLI's wizard for a name that was
// seeded, in front of Resolve for one the operator supplied (P5-R21: Resolve
// is where a kind does work it cannot take back), in front of the apply as the
// last net, and on admind's handoff-BEGIN route, which sends the operator to
// an external service that mints something no API will list afterwards. (Its
// submit route is not a fifth: that one reaches this through Finish.) No two
// of them refuse the same collision with different words.
func RefuseExisting(ctx context.Context, c client.Client, namespace, name string) error {
	name = strings.TrimSpace(name)
	if c == nil || name == "" {
		return nil
	}
	var existing spiceboxv1alpha1.Channel
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing)
	switch {
	case err == nil:
		return fmt.Errorf("channel %q already exists in namespace %q; choose a different name or delete it first", name, namespace)
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("check for existing Channel %q in namespace %q: %w", name, namespace, err)
	}
	return nil
}

// HandoffDriver runs one browser round trip and returns the answers it
// produced.
//
// It is a function the CLIENT supplies rather than a step this package takes,
// because the callback endpoint is the client's: the CLI opens a loopback
// listener and waits on it inside one call, while admind hands the operator a
// URL in one HTTP response and receives the callback in a later request. The
// two cannot be one implementation, and neither of them is a decision about
// ORDER — which is what this package owns.
//
// A driver that reports an error ENDS the run. Whether a failed round trip
// should instead detour to the kind's fallback questions is the driver's own
// judgement (the CLI's runHandoff makes it, and channelkinds.HandoffSpec.Begin
// documents the one failure that must never take that detour); by the time an
// error reaches Finish, that decision has already been made and the run is
// over.
type HandoffDriver func(ctx context.Context, spec *channelkinds.HandoffSpec, answers map[string]string) (map[string]string, error)

// Params is one run's tail: the kind, what it was asked with, everything the
// client collected, and how this client drives a handoff.
type Params struct {
	// Wizard is the kind's flow. Required.
	Wizard channelkinds.Wizard
	// In is the same WizardInput Inputs and Handoff were called with. Its K8s
	// and Namespace are what the collision check reads, and its Role is the
	// ChannelSpec.Role Finish stamps onto the produced Channel.
	//
	// THE ROLE IS READ FROM HERE AND NOT FROM A FIELD OF ITS OWN, which is
	// what makes "the role the kind was asked with" and "the role its Channel
	// is stamped with" the same value by construction. Held separately — as it
	// was — a client could stamp a role the kind's Inputs never saw, and a kind
	// that shapes its question set around the role (slack asks a role=output
	// Channel where to post) would declare the wrong set while the stamp came
	// out right. See channelkinds.WizardInput.Role.
	In channelkinds.WizardInput
	// Answers is every answer collected, keyed by Question.Name. Finish MERGES
	// into it — what the handoff produced, then what Resolve derived — so a
	// caller that needs the pre-run map intact passes a copy.
	Answers map[string]string
	// Handoff is what Wizard.Handoff returned, or nil when the kind declared
	// none (the common case). Passed in rather than re-asked, because a client
	// has to ask for it BEFORE the questions are rendered — its fallback
	// question names are part of the set a client accepts an answer for — and
	// asking twice would let a kind that does I/O there answer differently the
	// second time.
	Handoff *channelkinds.HandoffSpec
	// DriveHandoff is required when Handoff is non-nil and unused otherwise.
	DriveHandoff HandoffDriver
}

// Finish takes a complete answer map to the manifests the kind produces:
// refuse a name collision, drive the handoff, Resolve, Result.
//
// THE ORDER IS THE PAYLOAD OF THIS FUNCTION. Each step's position cost a bug
// to establish, and none of it is derivable from the channelkinds.Wizard
// contract:
//
//   - The collision check goes in FRONT of everything, because both of the
//     steps after it do work that cannot be taken back. Resolve is where
//     slack's provisioning route creates and installs a real Slack app, and
//     Slack has no API that lists a user's apps afterwards; the handoff is
//     where github's creates a real GitHub App. Checked only in front of the
//     apply — which is where it also still is, as the last net — an operator
//     who supplied a name that is already taken loses the run AND orphans an
//     app nothing can find again (P5-R21).
//
//   - It is skipped for a monitoring run, matching the CLI's own guard: that
//     flow deliberately reconfigures the Channel it matched, and its name
//     being taken is the point rather than the problem
//     (WizardOutput.ReplaceExisting).
//
//   - The handoff goes BEFORE Resolve, because what the handoff produces is
//     what Resolve reads: github's manual route answers with a PATH to the
//     App's key file, and turning that into the key itself is Resolve's job.
//
//   - Both the handoff's answers and Resolve's OVERWRITE what was collected: a
//     value an external service just minted, or one a kind just verified
//     against that service, is more authoritative than one a form or a flag
//     guessed at.
//
//   - The declared role (Params.In.Role) is stamped LAST, over what Result
//     produced, because it is the caller's decision and not the kind's — but
//     it is VALIDATED first, in front of everything, for the collision check's
//     reason: a role that cannot be stamped must not cost the operator an App
//     that already exists. The kind was ASKED with the same value, on the same
//     WizardInput, so a kind that shaped its questions around the role and the
//     stamp that lands on its Channel cannot disagree.
//
// Resolve's error is returned VERBATIM. It is the kind's own user-facing
// sentence ("Slack rejected this bot token (auth.test): …"), and wrapping it
// in a dispatcher-level phrase would bury the one line that says what the
// operator has to fix.
func Finish(ctx context.Context, p Params) (channelkinds.WizardOutput, error) {
	if p.Wizard == nil {
		return channelkinds.WizardOutput{}, errors.New("wizardrun: no wizard to finish")
	}
	if err := checkRole(p.In.Role, p.In.Monitoring); err != nil {
		return channelkinds.WizardOutput{}, err
	}
	if p.Answers == nil {
		// Every step below writes into it. A nil map would panic on the first
		// derived answer, which is a mode the common path never reaches.
		p.Answers = map[string]string{}
	}

	if !p.In.Monitoring {
		if err := RefuseExisting(ctx, p.In.K8s, p.In.Namespace, p.Answers[wizardkeys.KeyChannelName]); err != nil {
			return channelkinds.WizardOutput{}, err
		}
	}

	if p.Handoff != nil {
		// Validated HERE rather than only inside each client's driver, so a
		// malformed spec is refused by name on both routes: a nil Begin or
		// Complete is dereferenced by whichever client reaches it first.
		if err := ValidateHandoffSpec(p.Handoff); err != nil {
			return channelkinds.WizardOutput{}, err
		}
		if p.DriveHandoff == nil {
			return channelkinds.WizardOutput{}, errors.New(
				"wizardrun: this kind's setup includes a browser round trip and this client wired no way to drive one")
		}
		fromHandoff, err := p.DriveHandoff(ctx, p.Handoff, p.Answers)
		if err != nil {
			return channelkinds.WizardOutput{}, err
		}
		for k, v := range fromHandoff {
			p.Answers[k] = v
		}
	}

	derived, err := p.Wizard.Resolve(ctx, p.In, p.Answers)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	for k, v := range derived {
		p.Answers[k] = v
	}

	out, err := p.Wizard.Result(p.In, p.Answers)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	stampRole(&out, p.In.Role)
	return out, nil
}

// CheckRole reports whether a role a caller wants stamped is one this package
// will stamp — nil when it is, and the reason when it is not. An empty role is
// "leave the kind's own answer alone" and is always fine.
//
// EXPORTED SO AN EARLIER REFUSAL CAN AGREE WITH THIS ONE. Finish calls it in
// front of every run, which is the backstop every client gets; a client that
// can refuse SOONER should — `oap channel create` refuses before it even
// builds a cluster client, because a flag combination that was never going to
// work must not cost the operator a real Slack app. That client keeps its own
// WORDING (it can name --monitoring, which this package cannot) but takes its
// VERDICT from here, so the two cannot come to refuse different sets. A test
// in channelcmd pins the agreement over a shared input table; the same shape,
// and the same reason, as channelplan.RefuseNamedInstall being one function
// with two callers.
func CheckRole(role string, monitoring bool) error { return checkRole(role, monitoring) }

// checkRole refuses a declared role that cannot be stamped, before Finish
// takes a step it cannot take back.
//
// MONITORING IS NOT A DECLARABLE ROLE, in either direction. A monitoring
// Channel binds to no agent, and a run that wants one says so through
// WizardInput.Monitoring — which changes what the KIND does (slack has a
// separate monitoring flow that sets the role itself, and Finish skips the
// collision check because reconfiguring the matched Channel is the point).
// Naming the role as well would be a second way to say the same thing, and the
// two could disagree; naming it INSTEAD would drive the kind's agent flow and
// stamp "monitoring" onto its output, producing a Channel nothing routes
// anywhere. Both are refused here rather than left to whichever client
// remembers (channelplan's B-R12 refuses the declaration itself, one layer up;
// this is what makes the combination unrepresentable for every client).
//
// An unrecognised role is refused rather than stamped: the apiserver's enum
// would reject it at the apply, after the wizard has already run, in a message
// about a CRD field rather than about the caller's own input.
func checkRole(role string, monitoring bool) error {
	role = strings.TrimSpace(role)
	if role == "" {
		return nil
	}
	if monitoring {
		return fmt.Errorf(
			"wizardrun: this run builds a monitoring channel, which binds to no agent, so it cannot also be given role %q", role)
	}
	if role == spiceboxv1alpha1.ChannelRoleMonitoring {
		return fmt.Errorf(
			"wizardrun: %q is not a role a channel can be created WITH; a monitoring channel is asked for as one (WizardInput.Monitoring, `oap channel create --monitoring`), not by naming its role. Declare the role the agent actually uses (%s)",
			role, strings.Join(spiceboxv1alpha1.AgentChannelRoles(), "|"))
	}
	if !slices.Contains(spiceboxv1alpha1.AgentChannelRoles(), role) {
		return fmt.Errorf("wizardrun: %q is not a channel role; it must be one of %s",
			role, strings.Join(spiceboxv1alpha1.AgentChannelRoles(), "|"))
	}
	return nil
}

// stampRole puts the caller's declared role onto the Channel the kind
// produced. Empty leaves the kind's own answer alone; see channelkinds.WizardInput.Role for why
// "leave it alone" is not the same as "leave it unset".
//
// A nil ChannelManifest is nothing to stamp and not an error: a re-setup that
// only rewrote the credentials Secret legitimately produces no Channel, and
// the role of a Channel this run did not build is not this run's to state.
func stampRole(out *channelkinds.WizardOutput, role string) {
	role = strings.TrimSpace(role)
	if role == "" || out.ChannelManifest == nil {
		return
	}
	out.ChannelManifest.Spec.Role = role
}

// AllInputs is a kind's questions plus the ones its handoff would fall back
// on, in that order — the full set of keys a client may accept an answer for.
//
// It is one rule rather than each client's own because both clients CHECK an
// answer key against it and both BUILD their answer map from it, and the two
// halves of that have to agree. A handoff's fallback questions are questions
// this kind asks, on the route a headless host or a blocked port takes: a
// client deriving the set from Inputs alone refuses the very answer that makes
// those runs possible, and one that also drops them from its answer map hands
// Result a blank for a value the operator supplied.
//
// A nil handoff (the common case) returns inputs as it was handed them; a
// non-nil one is copied into a slice of this function's own rather than
// appended onto the kind's, which could write into whatever backs it.
func AllInputs(inputs []oap.Question, handoff *channelkinds.HandoffSpec) []oap.Question {
	if handoff == nil || len(handoff.FallbackInputs) == 0 {
		return inputs
	}
	all := make([]oap.Question, 0, len(inputs)+len(handoff.FallbackInputs))
	all = append(all, inputs...)
	return append(all, handoff.FallbackInputs...)
}

// ValidateHandoffSpec refuses a spec no client can drive, by name.
//
// Begin and Complete are called with no nil check on every route below this,
// and a fallback question a renderer cannot honor would only be discovered on
// the failure path — where it would replace the reason the handoff failed with
// a validation error. Both are programming errors in a kind; a named refusal
// beats a nil dereference and beats a surprise mid-detour.
func ValidateHandoffSpec(spec *channelkinds.HandoffSpec) error {
	if spec == nil {
		// Never reached from Finish, which drives a handoff only when the kind
		// returned one — but this function's whole job is to be the place a
		// malformed spec stops, and stopping a nil one by dereferencing it is
		// not that.
		return errors.New("handoff: no handoff to drive")
	}
	if spec.Begin == nil {
		return errors.New("handoff: the kind described a handoff with no Begin, so there is nowhere to send the operator")
	}
	if spec.Complete == nil {
		return errors.New("handoff: the kind described a handoff with no Complete, so there would be nothing to do with the callback")
	}
	// A SatisfiedBy key that names no question silently does nothing: the
	// question it was meant to stand in for is not there to be skipped, and
	// the one that IS there keeps being asked. That is the exact defect the
	// field exists to fix, so a typo in it is refused rather than inherited.
	declared := make(map[string]bool, len(spec.FallbackInputs))
	for _, q := range spec.FallbackInputs {
		declared[q.Name] = true
	}
	for name := range spec.SatisfiedBy {
		if !declared[name] {
			return fmt.Errorf("handoff: satisfiedBy names %q, which is not one of this kind's fallback questions", name)
		}
	}
	return channelkinds.ValidateInputs(spec.FallbackInputs)
}
