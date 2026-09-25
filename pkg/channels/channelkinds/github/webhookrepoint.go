package github

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/appprovision"
)

// Compile-time interface check.
var _ channelkinds.WebhookURLRepointer = Kind{}

// RepointWebhookURL implements channelkinds.WebhookURLRepointer: it sets the
// GitHub App's hook_attributes.url to url. It is the only write this kind
// makes against an existing App's configuration, it touches exactly that one
// field (see appprovision.UpdateWebhookURL), and the channel controller calls
// it only for a Channel carrying the provenance marker saying this tool
// registered the App.
//
// AN ADDRESS NOTHING OUTSIDE THIS MACHINE CAN REACH IS REFUSED HERE, at the
// outward-facing write, and not only wherever the caller derived it. GitHub
// accepts a hook URL it cannot reach on a PATCH — the reachability check runs
// at App CREATION — so a loopback written here does not fail, it succeeds and
// replaces a working webhook with one no delivery can ever arrive at, silently,
// for as long as nobody looks. The controller has its own reason to stand down
// before calling (see its externalBaseURL), and this stays regardless: the rule
// belongs where the write is, so a second caller cannot reintroduce it.
func (Kind) RepointWebhookURL(ctx context.Context, ch *spiceboxv1alpha1.Channel, secrets channelkinds.WebhookSecrets, url, providerAPIBaseURL string) error {
	if why := channelkinds.UnreachableWebhookURL(url); why != "" {
		return fmt.Errorf("github: Channel %s/%s: refusing to register %q as this App's webhook URL: %s",
			ch.Namespace, ch.Name, url, why)
	}
	appID, privateKey, client, err := appAPIClient(ch, secrets, providerAPIBaseURL, "write the App's webhook URL with")
	if err != nil {
		return err
	}
	if err := client.UpdateWebhookURL(ctx, appprovision.UpdateWebhookURLParams{
		AppID:         appID,
		PrivateKeyPEM: privateKey,
		URL:           url,
	}); err != nil {
		return fmt.Errorf("github: Channel %s/%s: %w", ch.Namespace, ch.Name, err)
	}
	return nil
}

// appAPIClient resolves the App-level credentials out of the Channel's
// credentials Secret and returns a client for the App-configuration API. Both
// the drift read (webhookdrift.go) and the repoint write above authenticate
// as the App itself with the same two Secret keys, so the extraction, the
// missing-key refusal, and the base-URL override live here once.
//
// purpose completes the missing-key error's sentence, so an operator reading
// it learns which call could not proceed.
//
// Neither the private key nor anything derived from it is placed in a
// returned error.
func appAPIClient(ch *spiceboxv1alpha1.Channel, secrets channelkinds.WebhookSecrets, providerAPIBaseURL, purpose string) (appID string, privateKey []byte, client *appprovision.HTTPClient, err error) {
	appID = string(secrets.Data["app-id"])
	privateKey = secrets.Data["private-key"]
	if appID == "" || len(privateKey) == 0 {
		return "", nil, nil, fmt.Errorf("github: Channel %s/%s: Secret has no app-id or private-key to %s",
			ch.Namespace, ch.Name, purpose)
	}
	var opts []appprovision.Option
	if providerAPIBaseURL != "" {
		opts = append(opts, appprovision.WithBaseURL(providerAPIBaseURL))
	}
	return appID, privateKey, appprovision.NewHTTPClient(opts...), nil
}
