package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/sessionviewlink"
)

// TestArtifactViewDeps_VerifyLink_SessionView proves artifactViewDeps.VerifyLink
// accepts a valid PurposeSessionView link — SessionRef only, no ArtifactID
// required — and returns the session ref + back-link with an empty
// artifactID (a session-view link never carries one).
func TestArtifactViewDeps_VerifyLink_SessionView(t *testing.T) {
	key := make([]byte, 32)
	signer := passthroughlink.New(key, passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd))
	d := &artifactViewDeps{cookieSigner: signer}

	m := &sessionviewlink.Minter{Signer: signer}
	backLink := "https://example.slack.com/archives/C1/p170?thread_ts=170&cid=C1"
	raw, err := m.Mint("default/sess-1", identity.RawSubject("user:alice"), backLink)
	require.NoError(t, err)

	artifactID, sessionRef, gotBackLink, err := d.VerifyLink(raw)
	require.NoError(t, err, "a valid session_view link must verify")
	assert.Empty(t, artifactID, "a session_view link carries no ArtifactID")
	assert.Equal(t, "default/sess-1", sessionRef)
	assert.Equal(t, backLink, gotBackLink)
}

// TestArtifactViewDeps_VerifyLink_SessionView_MissingSessionRefRejected proves
// VerifyLink fails closed when a PurposeSessionView payload has no
// SessionRef. sessionviewlink.Minter.Mint refuses to build such a token, so
// the payload is hand-built directly via the signer to exercise VerifyLink's
// own guard.
func TestArtifactViewDeps_VerifyLink_SessionView_MissingSessionRefRejected(t *testing.T) {
	key := make([]byte, 32)
	signer := passthroughlink.New(key, passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd))
	d := &artifactViewDeps{cookieSigner: signer}

	raw, err := signer.Mint(passthroughlink.Payload{
		Purpose:   passthroughlink.PurposeSessionView,
		Audience:  passthroughlink.AudienceWebd,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	_, _, _, err = d.VerifyLink(raw)
	require.Error(t, err, "a session_view link with no SessionRef must be rejected")
}

// TestArtifactViewDeps_VerifyLink_ArtifactViewStillRequiresBoth is a
// regression guard: adding the PurposeSessionView branch must NOT weaken the
// existing artifact_view verify path — it still requires BOTH ArtifactID and
// SessionRef.
func TestArtifactViewDeps_VerifyLink_ArtifactViewStillRequiresBoth(t *testing.T) {
	key := make([]byte, 32)
	signer := passthroughlink.New(key, passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd))
	d := &artifactViewDeps{cookieSigner: signer}

	// ArtifactID present, SessionRef missing — must still be rejected.
	raw, err := signer.Mint(passthroughlink.Payload{
		Purpose:    passthroughlink.PurposeArtifactView,
		Audience:   passthroughlink.AudienceWebd,
		ArtifactID: "art-1",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	_, _, _, err = d.VerifyLink(raw)
	require.Error(t, err, "artifact_view link missing SessionRef must still be rejected")
}

// TestArtifactViewDeps_VerifyLink_UnknownPurposeRejected proves an unrecognized
// Purpose is rejected rather than silently falling through to either branch.
func TestArtifactViewDeps_VerifyLink_UnknownPurposeRejected(t *testing.T) {
	key := make([]byte, 32)
	signer := passthroughlink.New(key, passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd))
	d := &artifactViewDeps{cookieSigner: signer}

	raw, err := signer.Mint(passthroughlink.Payload{
		Purpose:    "some_other_purpose",
		Audience:   passthroughlink.AudienceWebd,
		SessionRef: "default/sess-1",
		ArtifactID: "art-1",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	_, _, _, err = d.VerifyLink(raw)
	require.Error(t, err, "an unrecognized link purpose must be rejected")
}
