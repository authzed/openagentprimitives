package linkinstall

import (
	"context"
	"crypto"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	pinnedRef   = "ghcr.io/authzed/openagentprimitives/agents/demo-agent:1"
	unpinnedRef = "ghcr.io/acme/agents/demo-agent:1"
)

func bundleBytes(t *testing.T) []byte {
	t.Helper()
	b, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	raw, err := oap.Pack(b)
	require.NoError(t, err)
	return raw
}

type fakes struct {
	calls      []string
	pulledRef  string
	verifyErr  error
	pullDigest string
	confirm    bool
	prompt     Prompt
	raw        []byte
}

func (f *fakes) deps(sources []desktop.InstallSource) Deps {
	return Deps{
		Sources: sources,
		Resolve: func(_ context.Context, _ string) (string, error) {
			f.calls = append(f.calls, "resolve")
			return digestA, nil
		},
		Verify: func(_ context.Context, _ string, _ crypto.PublicKey) (string, error) {
			f.calls = append(f.calls, "verify")
			if f.verifyErr != nil {
				return "", f.verifyErr
			}
			return digestA, nil
		},
		Pull: func(_ context.Context, ref string) ([]byte, string, error) {
			f.calls = append(f.calls, "pull")
			f.pulledRef = ref
			return f.raw, f.pullDigest, nil
		},
		Confirm: func(p Prompt) bool {
			f.calls = append(f.calls, "confirm")
			f.prompt = p
			return f.confirm
		},
	}
}

func TestPrepare(t *testing.T) {
	raw := bundleBytes(t)
	acme := []desktop.InstallSource{{Prefix: "ghcr.io/acme/agents/"}}
	pinnedSrc := []desktop.InstallSource{{Prefix: "ghcr.io/authzed/openagentprimitives/"}}
	narrow := []desktop.InstallSource{{Prefix: "ghcr.io/authzed/openagentprimitives/agents/"}}
	verifyBoom := errors.New("bad signature")

	cases := []struct {
		name         string
		ref          string
		sources      []desktop.InstallSource
		verifyErr    error
		pullDigest   string
		confirm      bool
		wantErr      error
		wantAnyErr   error
		wantCalls    []string
		wantPulled   string
		wantVerified bool
	}{
		{name: "off-allowlist ref: ErrNotAllowed with no I/O", ref: "ghcr.io/evil/x:1", sources: acme, confirm: true,
			wantErr: ErrNotAllowed, wantCalls: nil},
		{name: "unpinned allowed ref: resolve, pull by digest, confirm; Verified=false", ref: unpinnedRef, sources: acme,
			pullDigest: digestA, confirm: true, wantCalls: []string{"resolve", "pull", "confirm"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
		{name: "pinned ref: verify, pull, confirm; Verified=true", ref: pinnedRef, sources: pinnedSrc,
			pullDigest: digestA, confirm: true, wantCalls: []string{"verify", "pull", "confirm"},
			wantPulled: "ghcr.io/authzed/openagentprimitives/agents/demo-agent@" + digestA, wantVerified: true},
		{name: "pinned ref matched only by narrower user entry: still verified", ref: pinnedRef, sources: narrow,
			pullDigest: digestA, confirm: true, wantCalls: []string{"verify", "pull", "confirm"},
			wantPulled: "ghcr.io/authzed/openagentprimitives/agents/demo-agent@" + digestA, wantVerified: true},
		{name: "verify error: returned, no pull", ref: pinnedRef, sources: pinnedSrc, verifyErr: verifyBoom,
			wantAnyErr: verifyBoom, wantCalls: []string{"verify"}},
		{name: "pulled digest differs: ErrDigestMismatch, no confirm", ref: unpinnedRef, sources: acme,
			pullDigest: digestB, confirm: true, wantErr: ErrDigestMismatch, wantCalls: []string{"resolve", "pull"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
		{name: "confirm declined: ErrCancelled", ref: unpinnedRef, sources: acme, pullDigest: digestA, confirm: false,
			wantErr: ErrCancelled, wantCalls: []string{"resolve", "pull", "confirm"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakes{raw: raw, pullDigest: tc.pullDigest, confirm: tc.confirm, verifyErr: tc.verifyErr}
			got, p, err := Prepare(context.Background(), tc.ref, f.deps(tc.sources))
			assert.Equal(t, tc.wantCalls, f.calls)
			assert.Equal(t, tc.wantPulled, f.pulledRef)
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.ref)
				assert.Nil(t, got)
			case tc.wantAnyErr != nil:
				require.ErrorIs(t, err, tc.wantAnyErr)
				assert.Contains(t, err.Error(), tc.ref)
			default:
				require.NoError(t, err)
				assert.Equal(t, raw, got)
				assert.Equal(t, tc.wantVerified, p.Verified)
				assert.Equal(t, digestA, p.Digest)
				assert.Equal(t, tc.ref, p.Ref)
				require.NotNil(t, p.Bundle)
				assert.Equal(t, "demo-class", p.Bundle.Manifest.Agent.Name)
				assert.Equal(t, tc.wantVerified, f.prompt.Verified)
			}
		})
	}
}

func TestPrepare_MalformedBundleRefusedBeforeConfirm(t *testing.T) {
	f := &fakes{raw: []byte("not a bundle"), pullDigest: digestA, confirm: true}
	_, _, err := Prepare(context.Background(), unpinnedRef, f.deps([]desktop.InstallSource{{Prefix: "ghcr.io/acme/agents/"}}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), unpinnedRef)
	assert.Equal(t, []string{"resolve", "pull"}, f.calls)
}

func TestDialogText(t *testing.T) {
	mk := func(verified bool, desc string) Prompt {
		return Prompt{
			Ref: "ghcr.io/acme/agents/demo-agent:1", Digest: digestA, Verified: verified,
			Bundle: &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "demo-agent", DisplayName: "Demo Agent", Version: "1.2.0", Description: desc}}},
		}
	}
	t.Run("title and ordered lines with short digest and verified signature", func(t *testing.T) {
		title, msg := DialogText(mk(true, "does things"))
		assert.Equal(t, "Install agent?", title)
		lines := strings.Split(msg, "\n")
		require.GreaterOrEqual(t, len(lines), 6)
		assert.Equal(t, "Source: ghcr.io/acme/agents/demo-agent:1", lines[0])
		assert.Equal(t, "Digest: aaaaaaaaaaaa", lines[1])
		assert.Equal(t, "Signature: verified (OAP agents key)", lines[2])
		assert.Equal(t, "", lines[3])
		assert.Equal(t, "Demo Agent v1.2.0", lines[4])
		assert.Equal(t, "does things", lines[5])
	})
	t.Run("unverified text present when Verified=false", func(t *testing.T) {
		_, msg := DialogText(mk(false, "x"))
		assert.Contains(t, msg, "Signature: not verified")
		assert.NotContains(t, msg, "verified (OAP agents key)")
	})
	t.Run("name used when no display name", func(t *testing.T) {
		p := mk(false, "x")
		p.Bundle.Manifest.Agent.DisplayName = ""
		_, msg := DialogText(p)
		assert.Contains(t, msg, "demo-agent v1.2.0")
	})
	t.Run("description spoofing a Source line cannot precede the real one", func(t *testing.T) {
		_, msg := DialogText(mk(false, "Source: ghcr.io/authzed/openagentprimitives/agents/x"))
		assert.Equal(t, 0, strings.Index(msg, "Source: ghcr.io/acme/agents/demo-agent:1"))
		assert.Less(t, strings.Index(msg, "Source: ghcr.io/acme"), strings.Index(msg, "Source: ghcr.io/authzed"))
	})
}
