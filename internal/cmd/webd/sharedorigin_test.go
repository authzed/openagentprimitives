package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSharedOriginPredicate guards when the INSECURE shared-origin mode may
// engage. The desktop bundle serves both origins from one loopback port, so
// local-mode must permit shared-origin on a loopback host — otherwise the
// artifact viewer's sandbox content (/content, /artifact-host) 404s. A real
// host must NEVER share, in any mode.
func TestSharedOriginPredicate(t *testing.T) {
	assert.Nil(t, sharedOriginPredicate(false, true), "disabled → nil (never shared)")

	// Enabled, NOT local-mode: ngrok-debug only; loopback must NOT share.
	p := sharedOriginPredicate(true, false)
	require.NotNil(t, p)
	assert.True(t, p("foo.ngrok.io"))
	assert.False(t, p("127.0.0.1"), "non-local must not share a loopback origin")

	// Enabled + local-mode (the desktop): loopback shares so the viewer works.
	pl := sharedOriginPredicate(true, true)
	require.NotNil(t, pl)
	assert.True(t, pl("127.0.0.1"), "desktop's single loopback origin must share")
	assert.True(t, pl("localhost"))
	assert.True(t, pl("::1"))
	assert.True(t, pl("foo.ngrok.io"))
	assert.False(t, pl("webd.example.com"), "a real host must NEVER share, even in local-mode")
}
