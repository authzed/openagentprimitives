package github

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/appprovision"
)

// Compile-time interface check.
var _ channelkinds.WebhookURLDriftChecker = Kind{}

// CheckWebhookURLDrift implements channelkinds.WebhookURLDriftChecker: it
// reads back the GitHub App's currently-registered webhook URL and compares
// it to expectedURL, which the caller (the channel controller) built with
// channelevents.WebhookPathFor. This call NEVER writes anything back to
// GitHub. Correcting a mismatch is a separate capability with its own
// authorization rule — see RepointWebhookURL (webhookrepoint.go), which the
// controller invokes only for an App this tool registered.
func (Kind) CheckWebhookURLDrift(ctx context.Context, ch *spiceboxv1alpha1.Channel, secrets channelkinds.WebhookSecrets, expectedURL, providerAPIBaseURL string) (string, bool, error) {
	appID, privateKey, client, err := appAPIClient(ch, secrets, providerAPIBaseURL, "read the App's config with")
	if err != nil {
		return "", false, err
	}
	cfg, err := client.ReadAppConfig(ctx, appprovision.ReadAppConfigParams{
		AppID:         appID,
		PrivateKeyPEM: privateKey,
	})
	if err != nil {
		return "", false, fmt.Errorf("github: Channel %s/%s: %w", ch.Namespace, ch.Name, err)
	}

	return cfg.WebhookURL, cfg.WebhookURL != expectedURL, nil
}
