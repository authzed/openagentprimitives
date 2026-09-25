package channelwizard

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// recordingDriver notes which screens a run actually PRESENTED, and answers
// each one the way a person at a plain terminal would.
//
// Recording the PRESENTATION rather than the declared question set is the
// point: a question a kind declares but never puts to the operator costs them
// nothing, while one it does put to them that their route cannot use is the
// defect. Only the driver sees that difference.
type recordingDriver struct {
	inner         tui.Driver
	ids           []string
	mutated       *bool
	afterMutation bool
}

func (d *recordingDriver) Present(ctx context.Context, screenID string, g *huh.Group) error {
	if d.mutated != nil && *d.mutated {
		d.afterMutation = true
	}
	d.ids = append(d.ids, screenID)
	return d.inner.Present(ctx, screenID, g)
}

// TestRunChannelWizard_InteractiveProvisionRouteAsksHowToAuthenticate is the
// test the interactive provisioning route needed and did not have: every other
// test of it seeds `slackapp=provision` from a flag, so none observes what an
// operator who PICKS that row at the prompt is asked next.
//
// What they were asked was a bot token and an app token — the two credentials
// the route exists to mint — because the whole question set is stated before
// any of it is answered, and the route the set branches on had only ever been
// read from the flags.
//
// The run ends at the configuration token, which is a deterministic,
// network-free stopping point: the paste source refuses an empty token before
// anything reaches Slack. Reaching THAT refusal is half the claim on its own —
// it is only reachable once the run has taken the provisioning branch of
// Resolve.
func TestRunChannelWizard_InteractiveProvisionRouteAsksHowToAuthenticate(t *testing.T) {
	k, ok := registry.Get("slack")
	require.True(t, ok, "the slack kind must be registered in the oap binary")

	b := aptest.NewBundle(t, &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	})

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	th := tui.NewTheme(tui.Caps{})
	// The answers, in the order the screens come: the one AgentClass, the
	// third Slack-app row ("create one for me"), the capability list confirmed
	// as pre-checked, the first token source, and blanks for the rest.
	rec := &recordingDriver{inner: tui.Plain(strings.NewReader("1\n3\n0\n1\n\n\n"), io.Discard, th)}

	_, _, err = Run(context.Background(), k.Wizard(),
		channelkinds.WizardInput{K8s: b.Controller, Namespace: "default", Seeded: seeded.Values},
		"slack", st, "", seeded,
		tui.Options{Theme: th, Driver: rec})

	require.Error(t, err, "the run stops at the configuration token it was never given")
	assert.Contains(t, err.Error(), "no configuration token was supplied",
		"the run must have reached the provisioning branch of Resolve, not the manual one")

	assert.Contains(t, rec.ids, "app-token-source",
		`an operator who picks "create one for me" must be asked how to authenticate with Slack's app-configuration API`)
	assert.NotContains(t, rec.ids, "bot-token",
		"and must NOT be asked for the bot token this route exists to mint")
	assert.NotContains(t, rec.ids, "app-token",
		"nor for the app-level token")
}

// TestRunChannelWizard_InteractiveManualRouteStillPrintsTheManifest is the
// other direction of the same branch, and the one a fix aimed only at
// provisioning could quietly break: an operator who picks "show me the
// manifest" must still get the manifest and be asked for neither pair of
// credentials.
func TestRunChannelWizard_InteractiveManualRouteStillPrintsTheManifest(t *testing.T) {
	k, ok := registry.Get("slack")
	require.True(t, ok, "the slack kind must be registered in the oap binary")

	b := aptest.NewBundle(t, &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	})

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	th := tui.NewTheme(tui.Caps{})
	// The first Slack-app row is the manual one, and it is also the default,
	// so the "1" here states the choice rather than relying on silence.
	rec := &recordingDriver{inner: tui.Plain(strings.NewReader("1\n1\n0\n\n"), io.Discard, th)}

	_, _, err = Run(context.Background(), k.Wizard(),
		channelkinds.WizardInput{K8s: b.Controller, Namespace: "default", Seeded: seeded.Values},
		"slack", st, "", seeded,
		tui.Options{Theme: th, Driver: rec})

	require.Error(t, err, "the manual route hands its manifest over as the error that ends the run")
	assert.Contains(t, err.Error(), "display_information",
		"the app manifest is the whole product of this run")

	for _, id := range []string{"bot-token", "app-token", "app-token-source", "app-config-token"} {
		assert.NotContains(t, rec.ids, id,
			"the manual route ends before any credential exists, so it must ask for none of them")
	}
}
