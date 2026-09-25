package fake

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// newCredReqTestDriver creates an isolated Driver (and its Channel) for use in
// credential_request sender tests. Each test that needs isolation should call
// ResetAllDrivers in a t.Cleanup to avoid bleed between test runs.
func newCredReqTestDriver(t *testing.T, ns, name string) (*Driver, channelkinds.Deps) {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	return driverFor(ch), channelkinds.Deps{Channel: ch}
}

func TestCredentialRequestSender_RecordsAllFields(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	drv, _ := newCredReqTestDriver(t, "default", "credreq-ch1")
	s := newCredentialRequestSender(drv)

	payload := channelevents.CredentialRequestPayload{
		SessionRef:         "default/s1",
		RecipientCanonical: "user:alice@example.com",
		LinkURL:            "https://identityd.example.org/link?d=abc&sig=def",
		Items: []channelevents.CredentialRequestItem{
			{Credential: "github-token", Title: "GitHub", Why: "These services are needed for the work you've asked Triage Bot to do."},
			{Credential: "linear-oauth", Title: "Linear"},
			{Credential: "stripe-pat", Title: "Stripe"},
		},
	}
	env, err := channelevents.BuildEnvelope("default", "s1", channelevents.KindCredentialRequest, payload)
	require.NoError(t, err)

	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err)

	recs := drv.CredentialRequests()
	require.Len(t, recs, 1)
	assert.Equal(t, payload.SessionRef, recs[0].Payload.SessionRef)
	assert.Equal(t, payload.RecipientCanonical, recs[0].Payload.RecipientCanonical)
	assert.Equal(t, payload.LinkURL, recs[0].Payload.LinkURL)
	assert.Equal(t, payload.Items, recs[0].Payload.Items)
	assert.Equal(t, "default", recs[0].SessionRef.Namespace)
	assert.Equal(t, "s1", recs[0].SessionRef.Name)
}

func TestCredentialRequestSender_ItemsSliceIsIndependent(t *testing.T) {
	// Mutating the original Items slice after Send must not affect the record.
	// The JSON round-trip in BuildEnvelope+Unmarshal naturally produces an
	// independent copy; verify the record is stable regardless.
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	drv, _ := newCredReqTestDriver(t, "default", "credreq-ch2")
	s := newCredentialRequestSender(drv)

	payload := channelevents.CredentialRequestPayload{
		SessionRef: "default/s2",
		Items: []channelevents.CredentialRequestItem{
			{Credential: "github", Title: "GitHub"},
			{Credential: "linear-oauth", Title: "Linear"},
		},
	}
	env, err := channelevents.BuildEnvelope("default", "s2", channelevents.KindCredentialRequest, payload)
	require.NoError(t, err)

	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s2"}, env)
	require.NoError(t, err)

	recs := drv.CredentialRequests()
	require.Len(t, recs, 1)
	require.Len(t, recs[0].Payload.Items, 2)
	assert.Equal(t, "GitHub", recs[0].Payload.Items[0].Title)
	assert.Equal(t, "Linear", recs[0].Payload.Items[1].Title)
}

func TestCredentialRequestSender_WrongKindError(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	drv, _ := newCredReqTestDriver(t, "default", "credreq-ch3")
	s := newCredentialRequestSender(drv)

	// Build an envelope with a different kind — sender must reject it.
	env, err := channelevents.BuildEnvelope("default", "sess",
		channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "x"})
	require.NoError(t, err)

	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "sess"}, env)
	require.Error(t, err, "credential_request sender must reject non-credential-request envelope kinds")
}

func TestCredentialRequestSender_BadPayloadError(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	drv, _ := newCredReqTestDriver(t, "default", "credreq-ch4")
	s := newCredentialRequestSender(drv)

	// Craft an envelope with KindCredentialRequest but malformed payload.
	env := channelevents.Envelope{
		Kind:    channelevents.KindCredentialRequest,
		Payload: json.RawMessage(`not-valid-json`),
	}
	_, err := s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "sess"}, env)
	require.Error(t, err, "credential_request sender must error on unparseable payload")
}

// TestCredentialRequestSender_RecordsLinkButtons verifies that the
// Slice 3 ε8 LinkButtons field on CredentialRequestPayload survives
// the envelope marshal+unmarshal performed by the fake sender. E2E
// scenarios that assert on per-credential buttons rely on this.
func TestCredentialRequestSender_RecordsLinkButtons(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	drv, _ := newCredReqTestDriver(t, "default", "credreq-buttons")
	s := newCredentialRequestSender(drv)

	payload := channelevents.CredentialRequestPayload{
		SessionRef:         "default/sess-buttons",
		RecipientCanonical: "user:alice@example.com",
		LinkURL:            "https://identityd.example.org/link?d=xyz&sig=qrs",
		Items: []channelevents.CredentialRequestItem{
			{Credential: "linear-oauth", Title: "Linear"},
			{Credential: "github-pat", Title: "GitHub PAT", Why: "needed for the work"},
		},
		LinkButtons: []channelevents.CredentialRequestButton{
			{Label: "Connect linear", URL: "https://identityd.example.org/link/oauth/linear-oauth"},
			{Label: "Connect your accounts", URL: "https://identityd.example.org/link?d=xyz&sig=qrs"},
		},
	}
	env, err := channelevents.BuildEnvelope("default", "sess-buttons", channelevents.KindCredentialRequest, payload)
	require.NoError(t, err)

	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "sess-buttons"}, env)
	require.NoError(t, err)

	recs := drv.CredentialRequests()
	require.Len(t, recs, 1)
	require.Len(t, recs[0].Payload.LinkButtons, 2, "buttons survive marshal+unmarshal")
	assert.Equal(t, "Connect linear", recs[0].Payload.LinkButtons[0].Label)
	assert.Equal(t, "https://identityd.example.org/link/oauth/linear-oauth", recs[0].Payload.LinkButtons[0].URL)
	assert.Equal(t, "Connect your accounts", recs[0].Payload.LinkButtons[1].Label)
	assert.Equal(t, payload.LinkURL, recs[0].Payload.LinkButtons[1].URL)
}

func TestCredentialRequestSender_MultipleEnvelopes(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	drv, _ := newCredReqTestDriver(t, "default", "credreq-ch5")
	s := newCredentialRequestSender(drv)

	for i, ref := range []string{"default/s10", "default/s11", "default/s12"} {
		p := channelevents.CredentialRequestPayload{SessionRef: ref, RecipientCanonical: "user:alice@example.com"}
		env, err := channelevents.BuildEnvelope("default", "s1", channelevents.KindCredentialRequest, p)
		require.NoError(t, err)
		_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
		require.NoError(t, err, "send %d", i)
	}

	recs := drv.CredentialRequests()
	require.Len(t, recs, 3)
	assert.Equal(t, "default/s10", recs[0].Payload.SessionRef)
	assert.Equal(t, "default/s11", recs[1].Payload.SessionRef)
	assert.Equal(t, "default/s12", recs[2].Payload.SessionRef)
}

func TestKind_SubChannelSender_CredentialRequest(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "credreq-ch6"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	k := Kind{}
	sender := k.SubChannelSender(string(channelevents.KindCredentialRequest), channelkinds.Deps{Channel: ch})
	require.NotNil(t, sender, "SubChannelSender(%q) must return a non-nil sender", channelevents.KindCredentialRequest)

	// Confirm it actually records.
	p := channelevents.CredentialRequestPayload{
		SessionRef:         "default/wired",
		RecipientCanonical: "user:bob@example.com",
		LinkURL:            "https://identityd.example.org/link?d=xyz",
		Items: []channelevents.CredentialRequestItem{
			{Credential: "stripe-pat", Title: "Stripe", Why: "Payment processing is required."},
		},
	}
	env, err := channelevents.BuildEnvelope("default", "wired", channelevents.KindCredentialRequest, p)
	require.NoError(t, err)

	_, err = sender.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "wired"}, env)
	require.NoError(t, err)

	recs := DriverFor("default", "credreq-ch6").CredentialRequests()
	require.Len(t, recs, 1)
	assert.Equal(t, "default/wired", recs[0].Payload.SessionRef)
	assert.Equal(t, "user:bob@example.com", recs[0].Payload.RecipientCanonical)
}
