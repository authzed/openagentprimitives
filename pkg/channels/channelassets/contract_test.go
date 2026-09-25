package channelassets_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

func TestExecutionMode_String(t *testing.T) {
	assert.Equal(t, "InProcess", channelassets.ExecutionModeInProcess.String())
	assert.Equal(t, "PodSpawn", channelassets.ExecutionModePodSpawn.String())
}

func TestDeliveryMode_String(t *testing.T) {
	assert.Equal(t, "Standalone", channelassets.DeliveryStandalone.String())
	assert.Equal(t, "BundledOnly", channelassets.DeliveryBundledOnly.String())
}

func TestDeniedOutputMIMEs(t *testing.T) {
	assert.True(t, channelassets.IsDeniedOutputMIME("image/svg+xml"), "image/svg+xml must be in deny-list")
	assert.False(t, channelassets.IsDeniedOutputMIME("text/html"), "text/html must NOT be in deny-list")
}

func TestErrMalformedPayload_IsMatchable(t *testing.T) {
	wrapped := fmt.Errorf("payload truncated mid-tag near byte 42: %w", channelassets.ErrMalformedPayload)
	assert.True(t, errors.Is(wrapped, channelassets.ErrMalformedPayload))
}

func TestWarning_String(t *testing.T) {
	cases := []struct {
		name string
		w    channelassets.Warning
		want string
	}{
		{
			name: "unwrapped tag names count + note",
			w:    channelassets.Warning{Kind: "tag", Name: "main", Action: "unwrapped", Count: 1, Note: "content kept; class/id hook lost"},
			want: "unwrapped 1 <main> tag(s): content kept; class/id hook lost",
		},
		{
			name: "removed tag no note",
			w:    channelassets.Warning{Kind: "tag", Name: "iframe", Action: "removed", Count: 2},
			want: "removed 2 <iframe> tag(s)",
		},
		{
			name: "stripped attr",
			w:    channelassets.Warning{Kind: "attr", Name: "onclick", Action: "stripped", Count: 3},
			want: "stripped onclick from 3 element(s)",
		},
		{
			name: "stripped css construct",
			w:    channelassets.Warning{Kind: "css", Name: "@import", Action: "stripped", Count: 1, Note: "remote stylesheet"},
			want: "stripped 1 CSS @import: remote stylesheet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.w.String())
		})
	}
}
