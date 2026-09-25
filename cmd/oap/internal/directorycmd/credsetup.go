// The credential-creation half of `oap directory configure`.
//
// The wizard used to end at a select box built from the credentials that
// already existed, so an operator with none was told a credential was required
// and given no way to make one — the only route was hand-written AgentIdentity
// YAML plus `oap identity put-token`. This is the other route: the chosen
// kind declares which credential-setup flow mints its credential
// (relsync.CredentialSetup), and that flow's screens are presented INLINE, in
// the same run, over the same driver.
//
// Nothing here branches on a kind's name. The flow comes from the kind's own
// declaration, the provider from the catalog entry that declares that flow,
// and the write goes through the same setup.Store every other credential in
// this project is written by.
package directorycmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// keyIdentity is the State key the AgentIdentity name lands under.
//
// It cannot collide with keyKind/keyCredential/keyEndpoint or with a kind's
// own ConfigScreen key, and not because the names happen to differ: the setup
// phase runs on a State of its OWN (see runCredentialSetup), which is also
// what keeps --answer out of it.
const keyIdentity = "identity"

// setupNewCredential is the credentialScreen answer meaning "I have none; walk
// me through making one".
//
// It deliberately carries no "/". Every real answer credentialValue produces
// does, so the two can never be mistaken for each other, and splitCredential
// would refuse this value outright if it ever reached it — which it cannot,
// because RunWizard replaces it with the minted credential's own value before
// anything splits it.
const setupNewCredential = "set-up-a-new-credential"

// setupNewCredentialLabel is what the operator reads on that option.
const setupNewCredentialLabel = "＋ Set up a new credential…"

// NewCredential names where a setup flow's minted value is written.
//
// ProviderID and SubjectID ride along because they are what makes the stored
// credential attributable: setup.Store records them as the Secret's
// attestation, so a later reader knows which provider account this value
// authenticated as.
//
// They are not empty together. VerifyCredential stamps ProviderID whenever a
// provider resolved, whatever the verdict, so a flow whose provider declares
// no probe still carries it; SubjectID is empty unless the probe actually
// concluded and the provider declares a stable id field. setup.Store needs
// BOTH to record anything, so a credential with only the provider half
// records nothing rather than an attestation to nobody.
type NewCredential struct {
	// Identity is the AgentIdentity to hold it, created when absent.
	Identity string
	// Name is the spec.credentials[].name within that identity.
	Name string
	// ProviderID is the catalog provider the value was checked against, or "".
	ProviderID string
	// SubjectID is the provider's stable id for the account it authenticated
	// as, or "" when nothing was verified.
	SubjectID string
}

// credentialSetupRun is one inline credential-setup pass.
type credentialSetupRun struct {
	// kind is the relsync kind being configured — used only to name things in
	// errors and defaults, never branched on.
	kind string
	// flow and intent are what the kind's own relsync.CredentialSetup declared.
	flow, intent string
	// scopes are the permission lines the kind wants the flow to print, also
	// from its own declaration. Nil keeps the flow's own recommendation.
	scopes []string
	// prior is what a re-run already has configured, so the AgentIdentity
	// question can default to the identity already in use.
	prior relsync.ExistingConfig
	// deps carries the write.
	deps Deps
	// existing is every credential already present in this namespace — what
	// AvailableCredentials found. Consulted so a run cannot silently replace
	// one; see the conflict refusal in runCredentialSetup.
	existing []CredentialRef
	// runOpts presents the screens — already carrying the driver this whole
	// wizard shares, reframed onto the Credential step.
	runOpts tui.Options
	// seeded is WizardOpts.Answers, consulted only to refuse a run that tried
	// to supply this flow's answers from a flag. It is never read FROM.
	seeded map[string]string
}

// runCredentialSetup walks the operator through the chosen kind's declared
// credential-setup flow and returns the credential it created.
//
// The flow's screens run on a FRESH State, never the wizard's own. That is the
// structural half of "a secret must never be settable through --answer":
// WizardOpts.Answers seeds the wizard's State with arbitrary keys, and a flow's
// token screen is answered by its key — so seeding "token" into the state the
// flow reads would let a credential arrive from the command line, where it
// lands in shell history, in the process table, and in any answer file the
// command is replayed from. A separate State makes that unreachable rather
// than forbidden; refuseSeededSetupAnswers then says so out loud, because an
// answer silently ignored is an answer the user believes was used.
func runCredentialSetup(ctx context.Context, r credentialSetupRun) (CredentialRef, error) {
	if r.deps.StoreCredential == nil {
		// Unreachable from RunWizard, which does not offer the option without
		// it. Kept so this function stays correct standalone.
		return CredentialRef{}, fmt.Errorf(
			"cannot create a credential for %q: this run has nowhere to write one", r.kind)
	}
	flow, ok := builtins.Get(r.flow)
	if !ok {
		return CredentialRef{}, fmt.Errorf(
			"directory-sync kind %q names credential setup flow %q, which this build has not registered",
			r.kind, r.flow)
	}
	// A provider is REQUIRED, not best-effort. Every flow tolerates a nil one
	// — and that is exactly the hazard: with no provider there is no declared
	// token format to check a paste against and no live probe to run, so a
	// mistyped or long-dead value stores clean and fails later as an opaque
	// 403. Losing both gates silently, because a flow was named that no
	// catalog entry claims, is not a degraded run worth having.
	//
	// Resolved by the DECLARED direction (which provider names this builtin),
	// not by assuming a provider shares its id with its flow.
	prov, ok := provider.ByBuiltin(r.flow)
	if !ok {
		return CredentialRef{}, fmt.Errorf(
			"credential setup flow %q has no provider in the catalog, so a pasted value would be checked "+
				"against no declared token format and verified against nothing. Refusing rather than "+
				"storing whatever was typed", r.flow)
	}

	// The credential's name within the chosen identity is the flow's own name
	// — "github-pat", "slack-bot-token" — rather than another question. It is
	// stable and derived solely from the kind's declaration, which is what
	// keeps a re-run from accumulating a differently-named credential per run.
	credName := r.flow

	// Refused BEFORE anything is presented, and before the flow is described.
	// An answer this run cannot use must not cost the operator a question
	// first, and a credential typed at a prompt this run was always going to
	// refuse is a credential already in their shell history.
	if err := refuseSeededSetupAnswers(ctx, flow, r); err != nil {
		return CredentialRef{}, err
	}

	// The AgentIdentity: every flow composes its guidance around the identity
	// the credential is for, so the name has to be answered before the flow's
	// screens can be described at all.
	st := tui.NewState()
	st, err := tui.RunWith(ctx, []tui.Screen{identityScreen(r.kind, r.prior, credName)}, r.runOpts, st)
	if err != nil {
		return CredentialRef{}, tui.UserFacing(err)
	}
	identityName := strings.TrimSpace(st.Get(keyIdentity))
	if identityName == "" {
		// Fail closed. The question is pre-filled, so this means the run's
		// input ran out — which huh's accessible renderer reports as silence
		// rather than as a failure.
		return CredentialRef{}, errors.New("no AgentIdentity name was supplied to hold the new credential")
	}

	// REFUSE rather than overwrite. setup.Store updates the Secret value of a
	// credential that already carries this name, and the name is derived, not
	// chosen — so an AgentIdentity that already holds a "github-pat" for some
	// repo-write toolkit would have it replaced by a directory sync's
	// org-read token, and the toolkit would start failing with nothing in this
	// command's output mentioning a replacement.
	//
	// `oap identity setup` guards the same write with its own already-set-up
	// check plus --force. This surface has no --force, and inventing one for
	// a wizard whose whole job is the first credential would be answering a
	// question nobody asked; naming the conflict and stopping is what lets the
	// operator decide.
	//
	// The suggested command must actually RUN as pasted: put-token requires
	// --credential plus exactly one --from-* source (cmd/oap/internal/identitycmd/
	// put_token.go), so naming put-token alone hands the operator a usage error
	// at the moment they are already blocked. --credential is filled in because
	// this run already knows it; --from-stdin is the one --from-* source that
	// needs no further argument here and, unlike --from-literal, never puts the
	// token itself in shell history — the right default for a human pasting at
	// a terminal, which is who reads this message.
	if credentialAvailable(r.existing, credentialValue(CredentialRef{Identity: identityName, Credential: credName})) {
		return CredentialRef{}, fmt.Errorf(
			"AgentIdentity %q already has a credential named %q, and setting one up here would replace "+
				"its value — which would break whatever else uses it. Choose a different identity, or "+
				"update the existing credential in place with `oap identity put-token %s --credential %s "+
				"--from-stdin` (paste the new token, then press Ctrl-D)",
			identityName, credName, identityName, credName)
	}

	stored := 0
	req := builtins.Request{
		Provider:     prov,
		UserIntent:   r.intent,
		ScopeHint:    r.scopes,
		IdentityName: identityName,
		// Kind, Target and Requirement are deliberately unset: they are the
		// toolkit/MCP vocabulary of `oap identity setup`, and a directory sync
		// is neither. Every flow reachable from here reads only the four fields
		// above plus Store — asserted, not assumed, by
		// TestRunWizard_SetupRunsForEveryKindThatDeclaresAFlow.
		//
		// Namespace and K8s are unset for one reason between them: the write is
		// Deps.StoreCredential's, and it already knows where it lands. Telling
		// the flow a namespace of our own would be a SECOND answer to "which
		// namespace", free to disagree with the one the write actually uses;
		// handing it a client would be a second write path into the same
		// AgentIdentity. One fact, one holder.
		Store: func(ctx context.Context, v builtins.StoreValue) error {
			res := builtins.VerifyCredential(ctx, prov, v)
			switch res.Status {
			case builtins.VerifyValid, builtins.VerifyForbidden,
				builtins.VerifyIndeterminate, builtins.VerifyUnsupported:
				// Stored. Only VerifyValid is proof; the other three are
				// explicitly non-verdicts (see VerifyStatus' own docs), and a
				// check that could not conclude must never block a credential.
			case builtins.VerifyRejected:
				// The provider's auth layer definitively refused it. Storing
				// anyway buys a RelationshipSource that reports Ready=False
				// forever, so refuse here where the operator can paste another.
				return fmt.Errorf(
					"that credential was rejected: %s. Nothing was stored — re-run to paste another", res.Detail)
			default:
				// Exhaustiveness backstop. A verdict this build does not
				// recognise is not evidence of anything, and there is no
				// confirmation step on this surface to ask a human with, so
				// refuse rather than let the NEXT verdict added fall open.
				return fmt.Errorf(
					"verification returned %q (%s), which this build does not recognise. Nothing was stored",
					res.Status, res.Detail)
			}
			stored++
			return r.deps.StoreCredential(ctx, NewCredential{
				Identity:   identityName,
				Name:       credName,
				ProviderID: res.ProviderID,
				SubjectID:  res.SubjectID,
			}, v)
		},
	}

	screens, err := flow.Screens(ctx, req)
	if err != nil {
		return CredentialRef{}, fmt.Errorf("credential setup for %q: %w", r.kind, err)
	}
	if len(screens) == 0 {
		// The Flow contract says a nil error comes with at least one screen; a
		// flow that returned neither would go on to Result against a State
		// nobody answered.
		return CredentialRef{}, fmt.Errorf(
			"credential setup flow %q described no screens and gave no reason", r.flow)
	}

	if st, err = tui.RunWith(ctx, screens, r.runOpts, st); err != nil {
		// Stripped here, at the call site that produced the framing: `tui:
		// apply screen "token":` in front of a message about a malformed token
		// is this command's plumbing showing through.
		return CredentialRef{}, tui.UserFacing(err)
	}

	if err := flow.Result(ctx, req, st); err != nil {
		return CredentialRef{}, fmt.Errorf("credential setup for %q: %w", r.kind, err)
	}
	if stored != 1 {
		// The flow reported success without writing anything. Returning a
		// CredentialRef here would name a credential that does not exist, and
		// the RelationshipSource written against it would never reconcile.
		return CredentialRef{}, fmt.Errorf(
			"credential setup flow %q reported success but stored %d credentials", r.flow, stored)
	}
	return CredentialRef{Identity: identityName, Credential: credName}, nil
}

// refuseSeededSetupAnswers refuses a run that tried to answer any of the
// credential-setup phase's questions from --answer.
//
// The phase runs on its own State, so such an answer is already inert — this
// does not close a hole, it makes an inert answer LOUD. Silently ignoring one
// is the worse failure: the operator believes the value was used, and has in
// the meantime put a credential into their shell history and their process
// table, where ignoring it does nothing to get it back out.
//
// It runs BEFORE any screen is presented, which means describing the flow's
// screens a second time, with a placeholder identity, purely to read the keys
// they declare. That is deliberate and it is cheap: Screens is a pure
// description with no I/O, the keys a flow asks under do not depend on which
// identity the credential is for, and the alternative is making the operator
// answer a question before being told the run was never going to accept their
// answer anyway.
func refuseSeededSetupAnswers(ctx context.Context, flow builtins.Flow, r credentialSetupRun) error {
	if len(r.seeded) == 0 {
		return nil
	}
	// keyIdentity is declared by this package rather than by the flow, and is
	// refused for the same reason even though it is not itself a secret: the
	// setup phase takes none of its answers from flags, and an operator who
	// supplied one and watched it be ignored learns nothing about why.
	declared := map[string]bool{keyIdentity: true}

	screens, err := flow.Screens(ctx, builtins.Request{
		Provider:     nil,
		UserIntent:   r.intent,
		ScopeHint:    r.scopes,
		IdentityName: placeholderIdentityName(r.kind, r.prior),
	})
	if err != nil {
		// Not fatal here: the real Screens call below reports it with the
		// context this one lacks. Refusing on the keys already known is still
		// correct, and is better than letting a seeded token through because a
		// description failed.
		screens = nil
	}
	for _, sc := range screens {
		a, ok := sc.(answerKeyer)
		if !ok {
			continue
		}
		for _, k := range a.AnswerKeys() {
			declared[k] = true
		}
	}

	var offending []string
	for k := range r.seeded {
		if declared[k] {
			offending = append(offending, k)
		}
	}
	if len(offending) == 0 {
		return nil
	}
	sort.Strings(offending) // map order is not an error message
	return fmt.Errorf(
		"--answer %s is not accepted: the credential setup phase takes none of its answers from flags, "+
			"because one of them is the credential itself — and a secret supplied as a flag lands in your "+
			"shell history, in the process table and in any answer file this command is replayed from. "+
			"Drop it and answer the prompts",
		strings.Join(offending, ", "))
}

// answerKeyer is implemented by a screen that asks the user for something.
//
// Duck-typed rather than imported, the same way cmd/oap/internal/channelwizard
// declares it: it is a single method, declared by tui.Question — which every
// screen these flows ask with is one of — so asking for it here couples this
// package to neither.
type answerKeyer interface {
	// AnswerKeys returns the State keys the screen may ask for on this run.
	AnswerKeys() []string
}

// identityScreen asks which AgentIdentity holds the new credential, defaulting
// to one derived from the kind so an operator can enter straight through.
//
// The guidance says what the run will actually do, which is narrower than the
// first draft claimed: the credential's name is DERIVED, not chosen, so naming
// an identity that already carries one under that name is a conflict rather
// than a merge, and runCredentialSetup refuses it. Saying so here is what
// stops an operator picking that identity to keep things tidy and only then
// being told no — and, before this was worded correctly, silently having a
// repo-write token replaced by an org-read one.
func identityScreen(kind string, prior relsync.ExistingConfig, credName string) tui.Screen {
	dflt := defaultIdentityName(kind, prior)
	return tui.NewText(tui.TextOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:    keyIdentity,
			Label: "Identity",
			Key:   keyIdentity,
			Title: "AgentIdentity to hold the new credential",
			Guidance: func(*tui.State) string {
				return "Credentials live on an AgentIdentity, created\n" +
					"here if it does not exist yet.\n\n" +
					"The new credential will be named:\n  " + credName + "\n\n" +
					"An identity that already has one by that name\n" +
					"is refused, not overwritten — pick another."
			},
			NoteLabel: "Identity",
		},
		Default: func() string { return dflt },
		Check:   checkIdentityName,
	})
}

// placeholderIdentityName is the identity name used only to DESCRIBE a flow's
// screens before the real one has been answered — see
// refuseSeededSetupAnswers. It is the same default the question is pre-filled
// with, so nothing composed from it is misleading, and no screen described
// with it is ever presented.
func placeholderIdentityName(kind string, prior relsync.ExistingConfig) string {
	return defaultIdentityName(kind, prior)
}

// defaultIdentityName is what the identity question is pre-filled with: the
// identity a re-run is already using, or one named after the kind.
//
// Derived solely from its inputs — no timestamp, no random suffix — so two
// runs that answer everything the same way name the same AgentIdentity and the
// wizard's own server-side apply stays a no-op.
func defaultIdentityName(kind string, prior relsync.ExistingConfig) string {
	if prior.Identity != "" {
		return prior.Identity
	}
	return kind + "-directory"
}

// checkIdentityName rejects a name Kubernetes will not accept, where the
// operator can retype it — rather than as an API-server rejection after the
// browser trip and the paste, with the token already in hand and nowhere to
// put it.
func checkIdentityName(_ *tui.State, v string) error {
	if errs := validation.IsDNS1123Subdomain(v); len(errs) > 0 {
		return fmt.Errorf("%q cannot be a Kubernetes object name: %s", v, strings.Join(errs, "; "))
	}
	return nil
}
