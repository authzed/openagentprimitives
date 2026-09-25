// The route an operator takes when the GitHub App ALREADY EXISTS, and the
// authority difference that follows from it.
//
// The security-relevant claim under test is one line long: an App the operator
// BRINGS must never carry channelkinds.AnnotationAppProvisionedBy, because
// that marker is what authorizes the channel controller to PATCH the App's
// webhook URL upstream (pkg/controllers/channel's provisionedByThisTool). An
// App we did not register is someone else's resource and only ever gets a
// drift finding.
package github

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// existingAppAnswers is a complete run by an operator who brought their own
// App: every up-front answer, the route set to "existing", and the four
// credentials plus the installation ID they read off its settings page.
//
// NO PROVENANCE MARKER, and that is the fixture's point rather than an
// omission — nothing on this route can produce one, because the only writer
// is the handoff's Complete and the handoff stands down here.
func existingAppAnswers(t *testing.T) map[string]string {
	t.Helper()
	f := answerFixtures(t)
	answers := provenanceBaseAnswers(t)
	answers[keyAppSource] = appSourceExisting
	for _, k := range []string{keyAppID, keySlug, keyPrivateKeyPEM, keyWebhookSecret} {
		answers[k] = f[k]
	}
	return answers
}

// ---------------------------------------------------------------------------
// the question itself
// ---------------------------------------------------------------------------

// TestInputs_AsksWhetherTheAppAlreadyExists pins the route question an
// operator reads, both halves separately: the VALUES a scripted
// `--answer app-source=…` writes, and the LABELS that are the whole of what an
// operator has to go on.
//
// The labels are asserted as literals and must SAY THE AUTHORITY DIFFERENCE.
// "I already have one" quietly meaning "and this tool may now rewrite its
// settings" is the worst outcome this route could have, so the choice states
// which of the two Apps this cluster may later change.
func TestInputs_AsksWhetherTheAppAlreadyExists(t *testing.T) {
	qs, err := (&wizard{}).Inputs(context.Background(), dataInput(t))
	require.NoError(t, err)
	q := questionNamed(t, qs, keyAppSource)

	assert.Equal(t, oap.QEnum, q.Type)
	assert.Equal(t, []string{"create", "existing"}, q.Enum,
		"the VALUES are what `--answer app-source=…` writes, so they are pinned")
	assert.Equal(t, []string{
		"No — create one now, and this run registers it under that owner",
		"Yes — I already have one, and I will give you its details",
	}, q.EnumLabels)
	assert.Nil(t, q.Default,
		"a default here would be a guess, silently accepted with a bare Enter, "+
			"that sends an operator who has an App to register a second one on github.com")

	assert.Contains(t, strings.ToLower(q.Description), "never changes its settings",
		"the operator must be told, where they choose, that an App they bring stays theirs")
}

// TestInputs_TheRouteIsSettledBeforeAnythingDerivedFromIt keeps the route
// question ahead of every question whose ANSWER only makes sense once it is
// settled — the credentials of an App that may or may not exist yet.
func TestInputs_TheRouteIsSettledBeforeAnythingDerivedFromIt(t *testing.T) {
	qs, err := (&wizard{}).Inputs(context.Background(), dataInput(t))
	require.NoError(t, err)

	names := questionNames(qs)
	require.Contains(t, names, keyAppSource)
	assert.Equal(t, 1, indexOfQuestion(names, keyAppSource),
		"the route is the second thing asked, right after the AgentClass it binds to")
}

func indexOfQuestion(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// the handoff stands down
// ---------------------------------------------------------------------------

// TestHandoff_StandsDownWhenTheOperatorBringsTheirOwnApp is the whole
// mechanism of the route: the browser round trip is what REGISTERS an App, and
// a run that already has one must not make a second.
//
// The reason is non-empty and operator-facing, because it is what a client
// puts above the questions that follow.
func TestHandoff_StandsDownWhenTheOperatorBringsTheirOwnApp(t *testing.T) {
	spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	why := spec.Skipped(map[string]string{keyAppSource: appSourceExisting})
	assert.NotEmpty(t, why, "an operator who brought an App must not be sent to create one")
	assert.Contains(t, strings.ToLower(why), "already",
		"the reason is read by the operator, so it says why they are not going to GitHub")

	assert.Empty(t, spec.Skipped(map[string]string{keyAppSource: appSourceCreate}),
		"the create route is the round trip; it must still run")
	assert.Empty(t, spec.Skipped(map[string]string{}),
		"an unanswered route is not a stand-down: the round trip is the floor this kind was built on")
}

// ---------------------------------------------------------------------------
// the authority difference — the security-relevant half
// ---------------------------------------------------------------------------

// TestResult_AnAppTheOperatorBringsIsNotMarkedAsOurs is THE test of this
// change. The marker authorizes an outward-facing PATCH of the App's webhook
// URL; an App we were merely handed the credentials of gets a drift finding
// instead, and a Channel that carried the marker would have this cluster
// rewriting a third party's App settings.
func TestResult_AnAppTheOperatorBringsIsNotMarkedAsOurs(t *testing.T) {
	out, err := (&wizard{}).Result(dataInput(t), existingAppAnswers(t))
	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)

	assert.NotContains(t, out.ChannelManifest.Annotations, channelkinds.AnnotationAppProvisionedBy,
		"an App the operator brought is someone else's resource; marking it would authorize repointing its webhook")
}

// TestResult_AnAppThisRunCreatedIsStillMarked is the regression direction of
// the test above: the route question must not have cost the automated route
// its provenance, which is what lets a later run correct the webhook URL when
// this cluster's external address changes.
func TestResult_AnAppThisRunCreatedIsStillMarked(t *testing.T) {
	f := answerFixtures(t)
	answers := provenanceBaseAnswers(t)
	answers[keyAppSource] = appSourceCreate
	for _, k := range []string{keyAppID, keySlug, keyPrivateKeyPEM, keyWebhookSecret} {
		answers[k] = f[k]
	}
	// Written by the exchange, which is the only thing that ever writes it.
	answers[keyProvisionedByOAP] = "true"

	out, err := (&wizard{}).Result(dataInput(t), answers)
	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, channelkinds.AppProvisionedByOAP,
		out.ChannelManifest.Annotations[channelkinds.AnnotationAppProvisionedBy])
}

// TestResult_RefusesToClaimWeCreatedAnAppTheOperatorBrought is the fail-closed
// guard behind the two above.
//
// The two facts cannot both be true, and today they cannot both arrive:
// keyProvisionedByOAP is written only by the handoff's Complete, and the
// handoff stands down on this route. That is a property of two functions
// agreeing, which nothing in the type system holds — so the one place the
// marker is stamped refuses the contradiction outright rather than resolving
// it in the direction that hands out authority.
func TestResult_RefusesToClaimWeCreatedAnAppTheOperatorBrought(t *testing.T) {
	answers := existingAppAnswers(t)
	answers[keyProvisionedByOAP] = "true"

	_, err := (&wizard{}).Result(dataInput(t), answers)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists",
		"the refusal names the contradiction rather than silently picking a side")
}

// ---------------------------------------------------------------------------
// what the operator reads on the route
// ---------------------------------------------------------------------------

// TestFallbackGuidance_BringingAnAppSaysWeWillNotChangeIt covers the
// operator-facing half of the authority difference. Someone who answers "I
// already have one" is about to hand over an App's private key; they are owed
// the sentence saying what this cluster will and will not do with it.
//
// It must also tell them what the App has to BE — the webhook URL this Channel
// answers on is derived from answers they cannot guess.
func TestFallbackGuidance_BringingAnAppSaysWeWillNotChangeIt(t *testing.T) {
	in := dataInput(t)
	in.WorkingDir = t.TempDir()
	spec, err := (&wizard{}).Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.NotNil(t, spec.FallbackGuidance)

	f := answerFixtures(t)
	answers := map[string]string{
		keyAppSource:              appSourceExisting,
		keyOwnerType:              ownerTypeOrg,
		keyOrg:                    f[keyOrg],
		keyExternalBaseURL:        f[keyExternalBaseURL],
		wizardkeys.KeyChannelName: f[wizardkeys.KeyChannelName],
	}
	guidance, err := spec.FallbackGuidance(answers)
	require.NoError(t, err)

	assert.Contains(t, guidance, "https://github.com/organizations/demo-org/settings/apps",
		"an org's Apps are listed under the organization, not under the operator's own settings")
	assert.Contains(t, guidance, "/webhooks/github/default/"+f[wizardkeys.KeyChannelName],
		"the webhook URL this Channel answers on is the one thing they cannot guess")
	assert.Contains(t, strings.ToLower(guidance), "drift",
		"a webhook URL that does not match is REPORTED, never corrected, on an App we did not register")
	assert.NotContains(t, strings.ToLower(guidance), "create the github app by hand",
		"this operator already has an App; the create-it-yourself steps are the other route's")
}

// TestFallbackGuidance_TheCreateRoutesAreUnchanged is the regression direction:
// the two routes that existed before still get their own text.
func TestFallbackGuidance_TheCreateRoutesAreUnchanged(t *testing.T) {
	in := dataInput(t)
	in.WorkingDir = t.TempDir()
	spec, err := (&wizard{}).Handoff(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, spec)

	f := answerFixtures(t)
	base := map[string]string{
		keyAppSource:              appSourceCreate,
		keyOwnerType:              ownerTypeOrg,
		keyOrg:                    f[keyOrg],
		keyExternalBaseURL:        f[keyExternalBaseURL],
		wizardkeys.KeyChannelName: f[wizardkeys.KeyChannelName],
	}

	// The App does not exist: the operator is about to create it by hand.
	manual, err := spec.FallbackGuidance(base)
	require.NoError(t, err)
	assert.Contains(t, manual, "Create the GitHub App by hand")

	// The App DOES exist because the exchange just created it: all that is
	// left is installing it.
	created := map[string]string{}
	for k, v := range base {
		created[k] = v
	}
	created[keyAppID] = f[keyAppID]
	created[keySlug] = f[keySlug]
	created[keyPrivateKeyPEM] = f[keyPrivateKeyPEM]
	created[keyWebhookSecret] = f[keyWebhookSecret]
	installOnly, err := spec.FallbackGuidance(created)
	require.NoError(t, err)
	assert.Contains(t, installOnly, "Install the App to its owner")
	assert.NotContains(t, installOnly, "Create the GitHub App by hand")
}

// ---------------------------------------------------------------------------
// change 2 — the installation page
// ---------------------------------------------------------------------------

// TestFallbackOpenURL_OpensTheInstallationPageOnceTheAppExists pins WHEN the
// installation page is worth opening and what address it is.
//
// It is fire-and-forget by construction: GitHub redirects an App's
// INSTALLATION back to a caller only when the App declares a Setup URL, and
// this App's manifest deliberately declares none (appprovision/manifest.go).
// So nothing waits on this and the installation ID is still asked for
// afterwards — which is why the address is named here rather than driven
// through Begin, whose whole shape is a callback this page will never send.
func TestFallbackOpenURL_OpensTheInstallationPageOnceTheAppExists(t *testing.T) {
	spec, err := (&wizard{}).Handoff(context.Background(), dataInput(t))
	require.NoError(t, err)
	require.NotNil(t, spec)

	f := answerFixtures(t)
	created := map[string]string{
		keyOwnerType:     ownerTypeOrg,
		keyOrg:           f[keyOrg],
		keyAppID:         f[keyAppID],
		keySlug:          f[keySlug],
		keyPrivateKeyPEM: f[keyPrivateKeyPEM],
		keyWebhookSecret: f[keyWebhookSecret],
	}
	assert.Equal(t, "https://github.com/apps/"+f[keySlug]+"/installations/new",
		spec.FallbackOpen(created),
		"the page that INSTALLS an App is scoped by its slug, and it is the page whose "+
			"\"Configure\" link carries the installation ID this run goes on to ask for")

	// Nothing to open before the App exists: this address 404s on a slug
	// nobody has registered, and the manual route's operator has not chosen a
	// name yet.
	assert.Empty(t, spec.FallbackOpen(map[string]string{
		keyOwnerType: ownerTypeOrg, keyOrg: f[keyOrg],
	}), "the manual route has no App to install yet")

	// The brought-App route has not been answered yet at the moment this is
	// read — the credentials ARE the questions about to be asked.
	assert.Empty(t, spec.FallbackOpen(map[string]string{keyAppSource: appSourceExisting}),
		"an App the operator is about to describe has no slug to build an address from")
}

// TestAppSettingsListURL_FollowsTheOwnerType pins the same two-address rule
// appCreateURL and installationsURL carry, for the page the brought-App route
// sends an operator to read their App's details off.
//
// An organization's Apps are listed under the organization; a personal
// account's under the account's own settings, with no login in the path at
// all. Sending one to the other's address is a 404, which is exactly the
// failure the owner-type question exists to prevent.
func TestAppSettingsListURL_FollowsTheOwnerType(t *testing.T) {
	assert.Equal(t, "https://github.com/organizations/demo-org/settings/apps",
		appSettingsListURL(ownerTypeOrg, "demo-org"))
	assert.Equal(t, "https://github.com/settings/apps",
		appSettingsListURL(ownerTypeUser, "demo-owner"),
		"a personal account has no /organizations/<login>/ tree at all")
}

// TestPostSetupNotes_SayWhoRegisteredTheApp records the authority difference
// where it outlives the run: the post-apply notes land in plain scrollback,
// long after the guidance screen is gone.
func TestPostSetupNotes_SayWhoRegisteredTheApp(t *testing.T) {
	brought, err := (&wizard{}).Result(dataInput(t), existingAppAnswers(t))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(brought.Notes, "\n"), "did not register",
		"an App we were handed is never repointed, and the operator is told so")

	f := answerFixtures(t)
	answers := provenanceBaseAnswers(t)
	answers[keyAppSource] = appSourceCreate
	for _, k := range []string{keyAppID, keySlug, keyPrivateKeyPEM, keyWebhookSecret} {
		answers[k] = f[k]
	}
	answers[keyProvisionedByOAP] = "true"
	created, err := (&wizard{}).Result(dataInput(t), answers)
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(created.Notes, "\n"), "did not register")
}

// ---------------------------------------------------------------------------
// the scripted routes
// ---------------------------------------------------------------------------

// TestUnattendedRefusal_NamesTheRightMissingFlagsForEachRoute keeps the
// unattended refusal honest on both routes. A run told the App already exists
// must not be advised to create one.
func TestUnattendedRefusal_NamesTheRightMissingFlagsForEachRoute(t *testing.T) {
	in := seededInput(t, map[string]string{keyAppSource: appSourceExisting})
	in.NonInteractive = true
	_, err := (&wizard{}).Inputs(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--answer "+keyAppID+"=",
		"a run that says it has the App is owed the flags that describe it")
	assert.NotContains(t, strings.ToLower(err.Error()), "creating a github app",
		"it already has one; telling it to create one is the wrong instruction")

	create := seededInput(t, map[string]string{keyAppSource: appSourceCreate})
	create.NonInteractive = true
	_, err = (&wizard{}).Inputs(context.Background(), create)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "creating a github app",
		"the create route still needs a human at a browser")
}

// TestSeededCredentialsStillSkipTheHandoffEntirely is the pre-existing
// scripted route: four credential flags and no route answer at all still means
// "this run has the App", because it demonstrably does.
func TestSeededCredentialsStillSkipTheHandoffEntirely(t *testing.T) {
	f := answerFixtures(t)
	in := seededInput(t, map[string]string{
		keyAppID:         f[keyAppID],
		keySlug:          f[keySlug],
		keyPrivateKeyPEM: f[keyPrivateKeyPEM],
		keyWebhookSecret: f[keyWebhookSecret],
	})
	spec, err := (&wizard{}).Handoff(context.Background(), in)
	require.NoError(t, err)
	assert.Nil(t, spec, "a run that already has the App must not be sent to create a second one")
}
