package interact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// b64 base64url-encodes an email for the "user:<b64>" canonical subject form,
// matching identity.DecodeForDisplay's expectations (see user_message_test.go).
func b64(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestAnnotationBatchKind_RegisteredAndPermission(t *testing.T) {
	k, ok := Get("annotation_batch")
	require.True(t, ok, "annotation_batch must be registered")
	assert.Equal(t, "interact", k.Permission())
}

func TestAnnotationBatchKind_EmptyBatchRefusedBeforeNATS(t *testing.T) {
	k, ok := Get("annotation_batch")
	require.True(t, ok, "annotation_batch must be registered")

	called := false
	deps := Deps{NATSRequest: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		called = true
		return nil, nil
	}}
	_, err := k.Submit(context.Background(), deps, "ns", "name",
		"user:"+b64("a@example.com"), "urn:ap:view:artifact:artifact-9",
		json.RawMessage(`{"annotations":[]}`))
	require.Error(t, err)
	assert.False(t, called, "an empty batch must be refused before any NATS request")
}

func TestAnnotationBatchKind_RoutesDeferredEchoBundle(t *testing.T) {
	k, ok := Get("annotation_batch")
	require.True(t, ok, "annotation_batch must be registered")

	var gotEnv channelevents.Envelope
	deps := Deps{NATSRequest: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		require.NoError(t, json.Unmarshal(data, &gotEnv))
		return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
	}}

	res, err := k.Submit(context.Background(), deps, "ns", "sess",
		"user:"+b64("a@example.com"), "urn:ap:view:artifact:artifact-9",
		json.RawMessage(`{"annotations":[
			{"target":"element","comment":"reword the hero","tagName":"button","elementPath":"#cta"},
			{"target":"region","comment":"why empty?","tagName":"section","elementPath":"#hero"}
		]}`))
	require.NoError(t, err, "Submit must succeed when NATS request routes the message")
	assert.Equal(t, "routed", res.Outcome)

	var pl channelevents.ViewMessagePayload
	require.NoError(t, json.Unmarshal(gotEnv.Payload, &pl))
	assert.True(t, pl.DeferEcho, "annotation batches must defer the raw echo")
	assert.Contains(t, pl.Text, "Annotation 1")
	assert.Contains(t, pl.Text, "<untrusted-annotations")
	assert.Equal(t, "urn:ap:view:artifact:artifact-9", pl.Via)
	assert.Equal(t, "a@example.com", pl.Author.Email.String(), "subject must decode back to the original email")
	assert.Equal(t, "idp", pl.Author.Kind.String())
}

func TestAnnotationBatchKind_TooManyAnnotationsRefusedBeforeNATS(t *testing.T) {
	k, ok := Get("annotation_batch")
	require.True(t, ok, "annotation_batch must be registered")

	called := false
	deps := Deps{NATSRequest: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		called = true
		return nil, nil
	}}

	anns := make([]map[string]string, maxAnnotations+1)
	for i := range anns {
		anns[i] = map[string]string{"target": "el", "comment": "x"}
	}
	raw, err := json.Marshal(map[string]any{"annotations": anns})
	require.NoError(t, err)

	_, err = k.Submit(context.Background(), deps, "ns", "name",
		"user:"+b64("a@example.com"), "urn:ap:view:artifact:artifact-9", raw)
	require.Error(t, err)
	assert.False(t, called, "a batch over the cap must be refused before any NATS request")
}

func TestAnnotationBatchKind_ClampsOversizedUntrustedFields(t *testing.T) {
	k, ok := Get("annotation_batch")
	require.True(t, ok, "annotation_batch must be registered")

	longText := make([]byte, maxDOMFieldChars+500)
	for i := range longText {
		longText[i] = 'x'
	}

	var gotEnv channelevents.Envelope
	deps := Deps{NATSRequest: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		require.NoError(t, json.Unmarshal(data, &gotEnv))
		return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
	}}

	raw, err := json.Marshal(map[string]any{
		"annotations": []map[string]string{
			{"target": "element", "comment": "reword", "elementText": string(longText)},
		},
	})
	require.NoError(t, err)

	_, err = k.Submit(context.Background(), deps, "ns", "sess",
		"user:"+b64("a@example.com"), "", raw)
	require.NoError(t, err)

	var pl channelevents.ViewMessagePayload
	require.NoError(t, json.Unmarshal(gotEnv.Payload, &pl))
	assert.NotContains(t, pl.Text, string(longText), "oversized untrusted DOM fields must be clamped before reaching the envelope")
}

func TestAnnotationBatchKind_ClampsOversizedChipFields(t *testing.T) {
	k, ok := Get("annotation_batch")
	require.True(t, ok, "annotation_batch must be registered")

	longIntent := make([]byte, maxChipChars+50)
	for i := range longIntent {
		longIntent[i] = 'y'
	}

	var gotEnv channelevents.Envelope
	deps := Deps{NATSRequest: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		require.NoError(t, json.Unmarshal(data, &gotEnv))
		return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
	}}

	raw, err := json.Marshal(map[string]any{
		"annotations": []map[string]string{
			{"target": "element", "comment": "reword", "intent": string(longIntent)},
		},
	})
	require.NoError(t, err)

	_, err = k.Submit(context.Background(), deps, "ns", "sess",
		"user:"+b64("a@example.com"), "", raw)
	require.NoError(t, err)

	var pl channelevents.ViewMessagePayload
	require.NoError(t, json.Unmarshal(gotEnv.Payload, &pl))
	assert.NotContains(t, pl.Text, string(longIntent), "an over-long intent chip field must be truncated before reaching the envelope")
}
