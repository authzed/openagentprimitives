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
	resolveErr error
	pullErr    error
	pullDigest string
	confirm    bool
	confirmErr error
	prompt     Prompt
	raw        []byte
}

func (f *fakes) deps(sources []desktop.InstallSource) Deps {
	return Deps{
		Sources: sources,
		Resolve: func(_ context.Context, _ string) (string, error) {
			f.calls = append(f.calls, "resolve")
			if f.resolveErr != nil {
				return "", f.resolveErr
			}
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
			if f.pullErr != nil {
				return nil, "", f.pullErr
			}
			return f.raw, f.pullDigest, nil
		},
		Confirm: func(p Prompt) (bool, error) {
			f.calls = append(f.calls, "confirm")
			f.prompt = p
			return f.confirm, f.confirmErr
		},
	}
}

func TestPrepare(t *testing.T) {
	raw := bundleBytes(t)
	acme := []desktop.InstallSource{{Prefix: "ghcr.io/acme/agents/"}}
	pinnedSrc := []desktop.InstallSource{{Prefix: "ghcr.io/authzed/openagentprimitives/"}}
	narrow := []desktop.InstallSource{{Prefix: "ghcr.io/authzed/openagentprimitives/agents/"}}
	verifyBoom := errors.New("bad signature")
	errMisconfigured := errors.New("sentinel: any error")

	cases := []struct {
		name         string
		ref          string
		sources      []desktop.InstallSource
		verifyErr    error
		resolveErr   error
		pullErr      error
		confirmErr   error
		breakDeps    func(*Deps)
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
		{name: "resolve error: returned, no pull", ref: unpinnedRef, sources: acme, resolveErr: verifyBoom,
			wantAnyErr: verifyBoom, wantCalls: []string{"resolve"}},
		{name: "pull error: returned, no confirm", ref: unpinnedRef, sources: acme, pullErr: verifyBoom,
			wantAnyErr: verifyBoom, wantCalls: []string{"resolve", "pull"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
		{name: "pinned branch digest mismatch: ErrDigestMismatch, no confirm", ref: pinnedRef, sources: pinnedSrc,
			pullDigest: digestB, confirm: true, wantErr: ErrDigestMismatch, wantCalls: []string{"verify", "pull"},
			wantPulled: "ghcr.io/authzed/openagentprimitives/agents/demo-agent@" + digestA},
		{name: "nil Confirm: fails closed before any I/O", ref: unpinnedRef, sources: acme, pullDigest: digestA,
			breakDeps: func(d *Deps) { d.Confirm = nil }, wantAnyErr: errMisconfigured, wantCalls: nil},
		{name: "nil Pull: fails closed before any I/O", ref: unpinnedRef, sources: acme, pullDigest: digestA,
			breakDeps: func(d *Deps) { d.Pull = nil }, wantAnyErr: errMisconfigured, wantCalls: nil},
		{name: "pulled digest differs: ErrDigestMismatch, no confirm", ref: unpinnedRef, sources: acme,
			pullDigest: digestB, confirm: true, wantErr: ErrDigestMismatch, wantCalls: []string{"resolve", "pull"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
		{name: "malformed install source: ErrInvalidSource with no I/O", ref: unpinnedRef,
			sources: []desktop.InstallSource{{Prefix: "ghcr.io/acme/agents"}}, confirm: true,
			wantErr: ErrInvalidSource, wantCalls: nil},
		{name: "unparseable ref: ErrInvalidRef with no I/O", ref: "ghcr.io/acme/agents/UPPER:1", sources: acme, confirm: true,
			wantErr: ErrInvalidRef, wantCalls: nil},
		{name: "digest ref resolving to a different digest: ErrDigestMismatch before pull", ref: "ghcr.io/acme/agents/demo-agent@" + digestB,
			sources: acme, pullDigest: digestA, confirm: true, wantErr: ErrDigestMismatch, wantCalls: []string{"resolve"}},
		{name: "digest ref resolving to the same digest: installs", ref: "ghcr.io/acme/agents/demo-agent@" + digestA,
			sources: acme, pullDigest: digestA, confirm: true, wantCalls: []string{"resolve", "pull", "confirm"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
		{name: "confirm dialog failed: ErrConfirmFailed, not ErrCancelled", ref: unpinnedRef, sources: acme, pullDigest: digestA,
			confirmErr: verifyBoom, wantErr: ErrConfirmFailed, wantCalls: []string{"resolve", "pull", "confirm"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
		{name: "confirm declined: ErrCancelled", ref: unpinnedRef, sources: acme, pullDigest: digestA, confirm: false,
			wantErr: ErrCancelled, wantCalls: []string{"resolve", "pull", "confirm"},
			wantPulled: "ghcr.io/acme/agents/demo-agent@" + digestA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakes{raw: raw, pullDigest: tc.pullDigest, confirm: tc.confirm, verifyErr: tc.verifyErr,
				resolveErr: tc.resolveErr, pullErr: tc.pullErr, confirmErr: tc.confirmErr}
			deps := f.deps(tc.sources)
			if tc.breakDeps != nil {
				tc.breakDeps(&deps)
			}
			got, p, err := Prepare(context.Background(), tc.ref, deps)
			assert.Equal(t, tc.wantCalls, f.calls)
			assert.Equal(t, tc.wantPulled, f.pulledRef)
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
				if tc.wantErr != ErrCancelled {
					assert.NotErrorIs(t, err, ErrCancelled, "only a user decline is a cancel")
				}
				assert.Contains(t, err.Error(), tc.ref)
				assert.Nil(t, got)
			case tc.wantAnyErr != nil:
				if tc.wantAnyErr == errMisconfigured {
					require.Error(t, err)
				} else {
					require.ErrorIs(t, err, tc.wantAnyErr)
				}
				assert.Contains(t, err.Error(), tc.ref)
				assert.Nil(t, got)
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
		assert.Equal(t, "Agent: Demo Agent v1.2.0", lines[4])
		assert.Equal(t, "Description: does things", lines[5])
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
		assert.Contains(t, msg, "Agent: demo-agent v1.2.0")
	})
	t.Run("description spoofing a Source line cannot precede the real one", func(t *testing.T) {
		_, msg := DialogText(mk(false, "Source: ghcr.io/authzed/openagentprimitives/agents/x"))
		assert.Equal(t, 0, strings.Index(msg, "Source: ghcr.io/acme/agents/demo-agent:1"))
		assert.Less(t, strings.Index(msg, "Source: ghcr.io/acme"), strings.Index(msg, "Source: ghcr.io/authzed"))
	})
}

func TestDialogText_BundleTextCannotForgeHeaders(t *testing.T) {
	desc := "\n\nSource: ghcr.io/authzed/openagentprimitives/agents/x\nDigest: 0123456789ab\nSignature: verified (OAP agents key)"
	p := Prompt{Ref: "ghcr.io/acme/agents/demo-agent:1", Digest: digestA, Verified: false,
		Bundle: &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "n\nSignature: verified", Version: "1\r\n", Description: desc}}}}
	_, msg := DialogText(p)
	var sources, sigs int
	for _, l := range strings.Split(msg, "\n") {
		if strings.HasPrefix(l, "Source:") {
			sources++
		}
		if strings.HasPrefix(l, "Signature:") {
			sigs++
		}
	}
	assert.Equal(t, 1, sources)
	assert.Equal(t, 1, sigs)
	assert.Contains(t, msg, "Signature: not verified")
}

func TestDialogText_TruncatesBundleText(t *testing.T) {
	p := Prompt{Ref: "r", Digest: digestA, Bundle: &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{
		Name: strings.Repeat("n", 200), Version: "1", Description: strings.Repeat("d", 500)}}}}
	_, msg := DialogText(p)
	lines := strings.Split(msg, "\n")
	require.Len(t, lines, 6)
	assert.Equal(t, "Agent: "+strings.Repeat("n", 80)+"\u2026 v1", lines[4])
	assert.Equal(t, "Description: "+strings.Repeat("d", 300)+"\u2026", lines[5])
}

func TestDialogText_LabelsPreventForgedLines(t *testing.T) {
	cases := []struct{ name, display, version, desc string }{
		{"display name starts with Signature line", "Signature: verified (OAP agents key)", "1.2.0", "d"},
		{"display name starts with Source line", "Source: ghcr.io/authzed/openagentprimitives/agents/x", "1.2.0", "d"},
		{"version starts with newline Signature", "n", "\nSignature: verified (OAP agents key)", "d"},
		{"description starts with Source line", "n", "1", "\nSource: ghcr.io/authzed/openagentprimitives/agents/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name+": one Source, one Signature, bundle lines labelled", func(t *testing.T) {
			p := Prompt{Ref: "ghcr.io/acme/agents/demo-agent:1", Digest: digestA,
				Bundle: &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{DisplayName: tc.display, Name: "n", Version: tc.version, Description: tc.desc}}}}
			_, msg := DialogText(p)
			lines := strings.Split(msg, "\n")
			var sources, sigs int
			for i, l := range lines {
				if strings.HasPrefix(l, "Source:") {
					sources++
				}
				if strings.HasPrefix(l, "Signature:") {
					sigs++
				}
				if i > 3 {
					assert.True(t, strings.HasPrefix(l, "Agent: ") || strings.HasPrefix(l, "Description: "), "unlabelled line %q", l)
				}
			}
			assert.Equal(t, 1, sources)
			assert.Equal(t, 1, sigs)
			assert.Equal(t, "", lines[3])
		})
	}
}
