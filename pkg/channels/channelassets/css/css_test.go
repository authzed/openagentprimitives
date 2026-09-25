package css_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	csskind "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
)

func TestRender_StripsDangerousConstructs(t *testing.T) {
	in := `@import url(http://evil.example/x.css); .a{color:red;background:url(https://evil.example/p.png)}`
	out, err := csskind.New().Render(context.Background(), channelassets.Input{Payload: []byte(in)})
	require.NoError(t, err)
	assert.Equal(t, "text/css", out.MIME)
	assert.NotContains(t, string(out.Bytes), "@import")
	assert.NotContains(t, string(out.Bytes), "evil.example")
	assert.NotEmpty(t, out.Warnings, "stripped constructs must be reported as warnings")
}

func TestRender_SafeCSSPassesThroughByteIdentical(t *testing.T) {
	in := `.panel { color: #333; margin: 0 auto; background: url(#grad); }`
	out, err := csskind.New().Render(context.Background(), channelassets.Input{Payload: []byte(in)})
	require.NoError(t, err)
	assert.Equal(t, in, string(out.Bytes), "safe CSS must be byte-identical")
	assert.Empty(t, out.Warnings)
}

func TestRendererContract(t *testing.T) {
	r := csskind.New()
	assert.Equal(t, "css", r.Kind())
	assert.Equal(t, channelassets.DeliveryBundledOnly, r.Delivery())
	assert.True(t, r.SupportsLiveView())
	assert.Equal(t, []string{"text/css"}, r.OutputMIMEs())
}
