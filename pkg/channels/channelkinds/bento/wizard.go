// Setup flow for `oap channel create --kind bento`. Inputs states five
// questions as data:
//
//	agentclass    pick the target AgentClass from existing CRs in the namespace
//	name          name the Channel resource
//	authzsubject  set the SpiceDB authzSubject (bento has no per-user attribution)
//	interval      set the Bento schedule
//	mapping       set the Bloblang mapping
//
// Result then emits an (empty) Secret + Channel manifest for the CLI to apply.
package bento

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// The answer keys this wizard declares. Stable strings: a caller seeding
// answers from flags for a non-interactive run addresses the questions by
// these.
const (
	keyInterval = "interval"
	keyMapping  = "mapping"
)

// The three shared keys, aliased so this package's Result and tests keep
// reading them by a local name. The STRINGS are wizardkeys' — a kind that
// retypes one silently stops being seeded the day the constant moves, and for
// the Channel name that also desynchronizes it from the collision check the
// dispatcher runs against wizardkeys.KeyChannelName.
const (
	keyAgentClass   = wizardkeys.KeyAgentClass
	keyChannelName  = wizardkeys.KeyChannelName
	keyAuthzSubject = wizardkeys.KeyAuthzSubject
)

// defaultInterval is the schedule a new bento Channel gets when the user
// accepts the default: weekly.
const defaultInterval = "@every 168h"

// defaultMapping is the Bloblang mapping a new bento Channel gets when the
// user accepts the default — a prompt telling them where to change it.
const defaultMapping = `root = "Weekly run — edit the Channel CR to customize."`

// The prompt text Inputs declares, as named constants rather than literals
// so a test asserting what an operator is asked compares against one string
// per question rather than two copies that can drift.
const (
	agentClassPrompt = "Bind to AgentClass"

	// This kind's own wording rather than wizardkeys.ChannelNamePrompt: bento's
	// batch already says "the Channel resource in the cluster" in the
	// description below, so the shared prompt's "resource" would say it twice.
	// The KEY is still the shared one — see the const block above for why that
	// half is not a choice.
	channelNamePrompt = "Channel name"
	// The Channel is a Kubernetes object, not a Slack-style room; say so
	// where the question is asked rather than in a preamble nobody reading
	// this question has in view.
	channelNameDescription = "The name of the Channel resource in the cluster."

	authzSubjectDescription = "Required: bento sessions are cron-spawned, so there is no per-user attribution to derive one from."

	intervalPrompt      = "Bento interval"
	intervalDescription = `Formats: "@every 168h", "0 9 * * MON", "@midnight"`

	mappingPrompt      = "Bloblang mapping"
	mappingDescription = "The message body each run sends to the agent."
)

// wizard holds nothing: every method takes the WizardInput it needs as an
// argument, which is what lets Result be called on a value no Inputs call
// ever touched — see channelkinds.Wizard.Result.
type wizard struct{}

// requiredBentoAnswers reads and validates the five answers Result needs,
// trimmed, failing closed on the first missing one — a blank Channel name or
// an empty AgentClass binding is worse than a refusal.
//
// get reads one answer by key rather than taking the map, so a caller with
// answers in another shape can still be held to the same check.
//
// The subject is checked for SHAPE and not only for presence, and this is the
// only place it can be: a channel wizard's questions carry no validation a
// client evaluates (channelkinds.ValidateInputs refuses a Question.Validation
// outright), so a malformed "service:…" would otherwise reach the apiserver
// and take the bound AgentClass to Valid=False. See
// wizardkeys.ValidateAuthzSubject.
func requiredBentoAnswers(get func(key string) string) (map[string]string, error) {
	answers := make(map[string]string, 5)
	for _, key := range []string{keyAgentClass, keyChannelName, keyAuthzSubject, keyInterval, keyMapping} {
		v := strings.TrimSpace(get(key))
		if v == "" {
			return nil, fmt.Errorf("bento wizard: %q was not answered", key)
		}
		answers[key] = v
	}
	if err := wizardkeys.ValidateAuthzSubject(answers[keyAuthzSubject]); err != nil {
		return nil, fmt.Errorf("bento wizard: %w", err)
	}
	return answers, nil
}

// bentoOutput builds the Secret + Channel manifests and next-steps notes from
// the five required answers. Summary is left to Result, which is where the
// run's decisions are stated — see channelkinds.WizardOutput.Summary.
func bentoOutput(namespace string, answers map[string]string) channelkinds.WizardOutput {
	channelName := answers[keyChannelName]
	secretName := channelName + "-creds"
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		// Bento channels need no credential keys; the Secret is schema-required
		// but left intentionally empty.
		Data: map[string][]byte{},
	}
	channel := &spiceboxv1alpha1.Channel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "agentprimitives.authzed.com/v1alpha1",
			Kind:       "Channel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      channelName,
			Namespace: namespace,
		},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:         "bento",
			Role:         spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:   answers[keyAgentClass],
			AuthzSubject: answers[keyAuthzSubject],
			SessionScope: "auto",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{
				SecretName: secretName,
			},
			Bento: &spiceboxv1alpha1.BentoChannelConfig{
				Generate: &spiceboxv1alpha1.BentoGenerateConfig{
					Mapping:  answers[keyMapping],
					Interval: answers[keyInterval],
				},
			},
		},
	}

	notes := []string{
		"Set AgentClass spec.sessionInteractPermission so cron-spawned sessions are reachable by humans.",
		fmt.Sprintf("Bento Channels don't need any keys in the credentials Secret, but the schema requires a CredentialsRef — apply an empty Secret named %q.", secretName),
		"Pair this with a kind=slack Channel (role=output) and SlackOutputDefaults to route the bot's reply.",
	}
	return channelkinds.WizardOutput{
		SecretManifest:  secret,
		ChannelManifest: channel,
		Notes:           notes,
	}
}

// Inputs states this kind's five questions: an enum listing the namespace's
// AgentClasses (the one part of this flow that does I/O) plus four free-text
// questions.
//
// "name" and "authzsubject" derive their default from the AgentClass that
// will be bound — but Inputs is called ONCE, before any answer exists, and
// questionscreen (the renderer these questions go through) bakes a
// question's Default into a closure over that single call's value; it never
// re-consults an answer a later question records.
//
// Ruling (P5-R16): a WRONG default is worse than NO default, because a wrong
// one can be silently accepted (a blank answer falls back to it) while an
// omitted one cannot (a required question with no Default fails closed — see
// the renderer's own missing-answer branch). So:
//   - Exactly one AgentClass in the namespace: classes[0] IS the unambiguous
//     answer, so "name"/"authzsubject" keep deriving from it.
//   - More than one: deriving from classes[0] would suggest a class the
//     operator may not have meant to pick — silently, since a blank answer
//     just accepts it — so the default is omitted instead and the question
//     becomes a hard required field. Naming the WRONG AgentClass in a bento
//     Channel is exactly the footgun this guards: the credentials Secret name
//     and any AgentIdentity in the same bundle are both derived from the
//     Channel name/AgentClass an operator might otherwise wave through.
//
// The AgentClass question's own Default is unaffected by this ruling: it
// always names classes[0] (or the seeded answer), because an enum's
// widget pre-selects its first option regardless of whether Default is
// set at all — there is no "omit the default" available for an enum the way
// there is for free text.
//
// A SEEDED AgentClass IS STILL VERIFIED AGAINST THE CLUSTER, the same as
// slack's and github's, because it is the same question and one answer to it
// is what wizardkeys.AgentClassQuestion exists to give. This kind used to skip
// both the question and the listing when a flag had answered it, so
// `--answer agentclass=does-not-exist` produced a Channel that lints clean and
// never works — the failure shape a Channel bound to a nonexistent AgentClass
// always has. An offline dry-run (nil in.K8s) has no cluster to check against
// and still accepts the seeded value; what changed is that a fully-seeded
// --non-interactive run against a REAL cluster now needs the AgentClass it
// names to exist there, which is the point.
func (w *wizard) Inputs(ctx context.Context, in channelkinds.WizardInput) ([]oap.Question, error) {
	if in.Namespace == "" {
		return nil, errors.New("namespace is required")
	}

	// unambiguous is true when defaultClass is known to be THE answer rather
	// than a guess: either the operator already named it (seeded), or it is
	// the only AgentClass that exists. Only then do "name"/"authzsubject" get
	// a derived default — see the ruling in the doc comment above.
	classQ, defaultClass, unambiguous, err := wizardkeys.AgentClassQuestion(ctx, in, agentClassPrompt)
	if err != nil {
		return nil, err
	}

	// nameDefault/subjectDefault are left as untyped nil (Question.Default's
	// zero value) when ambiguous, which is what makes the question required
	// with nothing to fall back on — see questionscreen.defaultString, which
	// reads a non-string Default (including nil) as "".
	var nameDefault, subjectDefault any
	if unambiguous {
		nameDefault = defaultClass + "-trigger"
		subjectDefault = "service:" + defaultClass + "-bot"
	}

	return []oap.Question{
		classQ,
		{
			Name:        keyChannelName,
			Type:        oap.QString,
			Prompt:      channelNamePrompt,
			Description: channelNameDescription,
			Default:     nameDefault,
		},
		{
			Name:        keyAuthzSubject,
			Type:        oap.QString,
			Prompt:      wizardkeys.AuthzSubjectPrompt,
			Description: authzSubjectDescription,
			Default:     subjectDefault,
		},
		{
			Name:        keyInterval,
			Type:        oap.QString,
			Prompt:      intervalPrompt,
			Description: intervalDescription,
			Default:     defaultInterval,
		},
		{
			Name:        keyMapping,
			Type:        oap.QString,
			Prompt:      mappingPrompt,
			Description: mappingDescription,
			Default:     defaultMapping,
		},
	}, nil
}

// Handoff sends the operator nowhere: every bento question is a plain value,
// with no external service to round-trip through.
func (w *wizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}

// Resolve derives nothing: bento holds no credential to verify, and every
// value its manifests need is already an answer.
func (w *wizard) Resolve(context.Context, channelkinds.WizardInput, map[string]string) (map[string]string, error) {
	return nil, nil
}

// Result reads the answers and produces the manifests, as a pure function of
// (in, answers) — see channelkinds.Wizard.Result for why it may not read
// anything a prior Inputs call left on the receiver.
//
// It carries the run's decisions in Summary because this kind runs no code of
// its own while the questions are being answered — see
// channelkinds.WizardOutput.Summary. None of bento's five answers is a
// credential, so none needs masking here — contrast slack's bot/app tokens,
// which do (see SummaryNote's doc).
func (w *wizard) Result(in channelkinds.WizardInput, answers map[string]string) (channelkinds.WizardOutput, error) {
	if in.Namespace == "" {
		return channelkinds.WizardOutput{}, errors.New("namespace is required")
	}
	resolved, err := requiredBentoAnswers(func(key string) string { return answers[key] })
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	out := bentoOutput(in.Namespace, resolved)
	out.Summary = []channelkinds.SummaryNote{
		{Label: "AgentClass", Value: resolved[keyAgentClass]},
		{Label: "Name", Value: resolved[keyChannelName]},
		{Label: "Subject", Value: resolved[keyAuthzSubject]},
		{Label: "Schedule", Value: resolved[keyInterval]},
		{Label: "Mapping", Value: resolved[keyMapping]},
	}
	return out, nil
}
