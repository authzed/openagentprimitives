package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestPublishLoginCaptchaNotice_PublishesNotice proves the CAPTCHA path surfaces a
// one-way login_captcha notice: category login_captcha, nil Actions (read-only),
// a NextStep (ToneUnavailable requires one), and the model-authored report
// carried inert in the Excerpt — never in the trusted Lead/Body.
func TestPublishLoginCaptchaNotice_PublishesNotice(t *testing.T) {
	const report = "a reCAPTCHA appeared <!channel> solve it"
	var published channelevents.Envelope
	l := &Loop{
		Status:     LocalStatusPatcher(),
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		InteractionRequestPublish: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			assert.Equal(t, "ns", ns)
			assert.Equal(t, "s", name)
			published = env
			return nil
		},
	}

	err := l.publishLoginCaptchaNotice(context.Background(), report, nil)
	require.NoError(t, err)

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	assert.Equal(t, categories.LoginCaptcha, pl.Category)
	assert.Nil(t, pl.Actions, "a captcha is a one-way NOTICE: no Done button, no decision leg")
	assert.NotEmpty(t, pl.NextStep, "ToneUnavailable requires a nextStep telling the reader to take over")
	assert.NotContains(t, pl.Lead, "<!channel>", "the model's report must never land in the trusted Lead")
	assert.NotContains(t, pl.Body, "<!channel>", "nor in the trusted Body")
	require.NotNil(t, pl.Excerpt, "the model's report rides the inert excerpt")
	assert.Contains(t, pl.Excerpt.Content, "<!channel>", "the report is carried verbatim, inert")
	assert.Equal(t, channelevents.AudienceParticipants, pl.Audience.Scope)
	assert.NoError(t, pl.Validate())
}

// TestPublishLoginCaptchaNotice_NoPublisher_Errors proves a session with no
// channel surface reports the failure rather than silently dropping it — the tool
// then tells the model, per no-silent-errors.
func TestPublishLoginCaptchaNotice_NoPublisher_Errors(t *testing.T) {
	l := &Loop{
		Status:     LocalStatusPatcher(),
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		// InteractionRequestPublish deliberately nil.
	}
	err := l.publishLoginCaptchaNotice(context.Background(), "a captcha appeared", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish hook")
}

// TestPublishLoginCaptchaNotice_AttachesScreenshot proves that when a screenshot
// capturer succeeds, the CAPTCHA notice's payload carries the screenshot artifact ref.
func TestPublishLoginCaptchaNotice_AttachesScreenshot(t *testing.T) {
	var published channelevents.Envelope
	l := &Loop{
		Status:     LocalStatusPatcher(),
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		Client: fake.NewClientBuilder().WithScheme(browserScreenshotTestScheme(t)).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build(),
		Artifacts: uiResourceTestArtifactsService(),
		InteractionRequestPublish: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			assert.Equal(t, "ns", ns)
			assert.Equal(t, "s", name)
			published = env
			return nil
		},
	}

	err := l.publishLoginCaptchaNotice(context.Background(), "a captcha appeared", &fakeCapturer{png: []byte("fakepngbytes")})
	require.NoError(t, err)

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	require.NotNil(t, pl.Screenshot, "a working capturer must attach a screenshot")
	assert.NotEmpty(t, pl.Screenshot.RenderName)
	assert.Equal(t, "image/png", pl.Screenshot.MIME)
}

// TestPublishLoginCaptchaNotice_CaptureError_PublishesTextOnly proves that
// when a screenshot capturer fails, the CAPTCHA notice still publishes but
// without a screenshot (best-effort capture, fail-closed publish).
func TestPublishLoginCaptchaNotice_CaptureError_PublishesTextOnly(t *testing.T) {
	var published channelevents.Envelope
	l := &Loop{
		Status:     LocalStatusPatcher(),
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		Client:     fake.NewClientBuilder().WithScheme(browserScreenshotTestScheme(t)).Build(),
		InteractionRequestPublish: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			assert.Equal(t, "ns", ns)
			assert.Equal(t, "s", name)
			published = env
			return nil
		},
	}

	err := l.publishLoginCaptchaNotice(context.Background(), "a captcha appeared", &fakeCapturer{err: errors.New("capture boom")})
	require.NoError(t, err, "capture error must not fail the notice publish")

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	assert.Nil(t, pl.Screenshot, "a capture error must leave screenshot nil")
	assert.NotEmpty(t, pl.Excerpt.Content, "the text notice must still be published")
}
