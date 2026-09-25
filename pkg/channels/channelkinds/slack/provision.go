// pkg/channels/channelkinds/slack/provision.go
//
// The provisioning step, run from Resolve: create the Slack app from the
// manifest this run's answers imply and install it to the workspace, so the
// two tokens the Channel needs are minted rather than copied out of a browser.
//
// Both the manifest it sends and the scopes it grants are derived from the
// capability answer, and one derivation feeding both is what keeps the scopes
// requested and the scopes granted from drifting.
package slack

import (
	"context"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/appprovision"
)

// The answer keys this step reads or derives.
const (
	keyTokenSource = "app-token-source"
	keyConfigToken = "app-config-token"
	// Derived below, not asked.
	keyAppID = "appid"
)

// The prompt text this step's questions declare, as named constants rather than
// literals so a test asserting what an operator is asked compares against one
// string per question rather than two copies that can drift.
const (
	tokenSourcePrompt = "How should oap authenticate with Slack?"
	configTokenPrompt = "App configuration token"
)

// unattendedReason reports why the named source cannot serve a run with nobody
// watching, or "" when it can — including for a source that has not been
// chosen yet, since an interactive run is free to pick any of them and the
// choice is what this question is about.
func unattendedReason(sourceKey string) string {
	if sourceKey == "" {
		return ""
	}
	src, ok := appprovision.SourceFor(sourceKey)
	if !ok {
		// An unknown key is not this check's to refuse: provisionConfigToken's
		// own SourceFor lookup reports it, naming the key, rather than this
		// one guessing at what the caller meant.
		return ""
	}
	return src.UnattendedReason()
}

// provisionConfigToken resolves the app-configuration token from the chosen
// source.
//
// unattended is what makes a source that waits on a person refuse rather than
// hang: it is the FAIL-CLOSED half of the same rule provisionUnattendedRefusal
// reads. That one is the friendly half — it runs from Inputs, before anything
// is asked, and names the flag that would help — but a client that never
// called Inputs reaches here anyway, and a source that waits on a person would
// then be handed a terminal nobody is at: captured output, no stdin, and a
// prompt that can only end in a confusing subprocess failure.
//
// operatorShell is the same shape of guard for the same shape of hazard, one
// step further out. provisionCredentialQuestions never OFFERS a shell-bound
// source to a client that has no shell, but the answer can also arrive as a
// flag, or from a client that skipped Inputs — and running the Slack CLI
// inside a serving container would exec a binary the operator cannot see, on a
// machine they are not at. Refused by name rather than attempted.
func provisionConfigToken(ctx context.Context, sourceKey, pasted string, unattended, operatorShell bool) (string, error) {
	src, ok := appprovision.SourceFor(sourceKey)
	if !ok {
		return "", fmt.Errorf("unknown token source %q", sourceKey)
	}
	if src.NeedsOperatorShell() && !operatorShell {
		return "", fmt.Errorf("the %q token source runs on the machine you are sitting at, and this run is not one: "+
			"use %q and supply the token with --answer %s=<token>",
			sourceKey, appprovision.KeyPaste, keyConfigToken)
	}
	if unattended {
		if why := src.UnattendedReason(); why != "" {
			return "", errors.New(why)
		}
	}
	return src.Token(ctx, pasted)
}

// provisionSlackApp creates the app from the manifest this run's capability
// answer implies and installs it, deriving both payloads from that one answer
// so the manifest's scopes and the install's scopes are one list.
//
// It returns the app ID of an ORPHAN — an app that was created and could not
// be finished — alongside the error, rather than recording it itself: no Slack
// API lists a user's apps, so that ID is the only record there will ever be of
// it, and the caller decides where it goes. agentResolve puts it in the error
// text, which is all a run that ends here has left.
func provisionSlackApp(
	ctx context.Context,
	c appprovision.Client,
	agentClass string,
	capabilities []string,
	token string,
) (appprovision.InstallResult, string, error) {
	if c == nil {
		return appprovision.InstallResult{}, "", errors.New("slack wizard: no app-provisioning client wired")
	}
	features := selectedFeaturesFrom(capabilities)
	manifestYAML := appManifestFor(agentClass+botDisplayNameSuffix, features)
	manifestJSON, err := appprovision.ManifestJSON(manifestYAML)
	if err != nil {
		return appprovision.InstallResult{}, "", err
	}

	created, err := c.Create(ctx, token, manifestJSON)
	if err != nil {
		return appprovision.InstallResult{}, "", err
	}

	scopes := channelkinds.ScopesFor(&Kind{}, features)
	installed, err := c.Install(ctx, token, created.AppID, scopes)
	if err != nil {
		return appprovision.InstallResult{}, created.AppID, fmt.Errorf(
			"the Slack app %q (%s) was created but could not be installed, so it is still in your workspace's app list: %w",
			agentClass+botDisplayNameSuffix, created.AppID, err)
	}
	if installed.BotToken == "" || installed.AppLevelToken == "" {
		return appprovision.InstallResult{}, created.AppID, fmt.Errorf(
			"Slack installed app %s but returned no %s token; Socket Mode cannot connect without both",
			created.AppID, missingTokenName(installed))
	}
	// apps.developerInstall is undocumented, so its response shape is not a
	// contract: an install that answers without app_id would otherwise leave
	// this run with an empty app ID, which is the same value a pasted-token run
	// has — and everything downstream reads that emptiness as "the user brought
	// their own tokens", from spec.slack.appId to the save-your-tokens warning
	// shown to someone who was never given a token to save. The ID we asked
	// Slack to install is authoritative for which app these tokens belong to.
	if installed.AppID == "" {
		installed.AppID = created.AppID
	}
	return installed, "", nil
}

// missingTokenName names which half of the install response was empty, so the
// error says what is missing rather than that something is.
func missingTokenName(r appprovision.InstallResult) string {
	if r.BotToken == "" {
		return "bot"
	}
	return "app-level"
}

// availableSources are the token sources that can actually run for THIS run.
//
// operatorShell is channelkinds.WizardInput.OperatorShell: whether the wizard
// is running in the operator's own shell. A source that needs one is dropped
// before Available is consulted, and the order is the whole point — Available
// probes the host the call lands on, which under admind is the serving
// container. Asking it first would compute the enum an operator READS from
// what happens to be installed in an image they never see, and then offer a
// route that execs a binary on the wrong machine and blocks on a person who is
// not at it.
//
// The paste source needs no shell, so a server-side client is never left with
// an empty set.
func availableSources(operatorShell bool) []appprovision.TokenSource {
	var out []appprovision.TokenSource
	for _, s := range appprovision.Sources() {
		if s.NeedsOperatorShell() && !operatorShell {
			continue
		}
		if s.Available() {
			out = append(out, s)
		}
	}
	return out
}

const provisionGuidance = `oap will create the Slack app for you and install it to your workspace.

  Generate a configuration token at https://api.slack.com/apps under
  "Your App Configuration Tokens". It is valid for 12 hours, it is used
  once here, and it is never stored.`
