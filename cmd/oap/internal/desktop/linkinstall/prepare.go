// Package linkinstall is the platform-neutral core between "an oap:// install
// link arrived" and "the user approved these exact bytes". Ordering is the
// security property: the allowlist is checked with zero I/O, the ref is then
// verified (pinned sources) or resolved (others) to a digest, the pull is by
// that digest, and the dialog is shown only for bytes whose digest matches.
package linkinstall

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/trustedkeys"
)

var (
	// ErrNotAllowed means the ref is outside every configured install source.
	ErrNotAllowed = errors.New("ref is not under an allowed install source")
	// ErrCancelled means the user declined the confirmation dialog.
	ErrCancelled = errors.New("install cancelled by user")
	// ErrDigestMismatch means the pulled content is not the verified/resolved digest.
	ErrDigestMismatch = errors.New("pulled digest differs from the verified digest")
)

// Prompt is what the user is asked to approve.
type Prompt struct {
	Ref, Digest string
	Verified    bool
	Bundle      *oap.Bundle
}

// Deps are the side-effecting collaborators, injected so Prepare is testable
// and platform-neutral.
type Deps struct {
	Sources []desktop.InstallSource
	Resolve func(ctx context.Context, ref string) (string, error)
	Verify  func(ctx context.Context, ref string, pub crypto.PublicKey) (string, error)
	Pull    func(ctx context.Context, pinnedRef string) ([]byte, string, error)
	Confirm func(Prompt) bool
}

// Prepare fetches and gets approval for ref, returning the approved bundle
// bytes. Every refusal wraps a sentinel or the underlying error and names ref.
func Prepare(ctx context.Context, ref string, d Deps) ([]byte, Prompt, error) {
	if d.Resolve == nil || d.Verify == nil || d.Pull == nil || d.Confirm == nil {
		return nil, Prompt{}, fmt.Errorf("install %q: misconfigured dependencies (nil Resolve/Verify/Pull/Confirm)", ref)
	}
	_, ok, err := desktop.AllowedSource(d.Sources, ref)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("install source check for %q: %w", ref, err)
	}
	if !ok {
		return nil, Prompt{}, fmt.Errorf("%q: %w", ref, ErrNotAllowed)
	}

	// Any error from RequiredKey is a refusal, even if it also reported pinned.
	pub, pinned, err := trustedkeys.RequiredKey(ref)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("trusted key lookup for %q: %w", ref, err)
	}

	var digest string
	if pinned {
		digest, err = d.Verify(ctx, ref, pub)
		if err != nil {
			return nil, Prompt{}, fmt.Errorf("verify %q: %w", ref, err)
		}
	} else {
		digest, err = d.Resolve(ctx, ref)
		if err != nil {
			return nil, Prompt{}, fmt.Errorf("resolve %q: %w", ref, err)
		}
	}

	pinnedRef, err := oci.PinnedRef(ref, digest)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("pin %q: %w", ref, err)
	}
	raw, pulled, err := d.Pull(ctx, pinnedRef)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("pull %q: %w", ref, err)
	}
	if pulled != digest {
		return nil, Prompt{}, fmt.Errorf("%q: pulled %s, expected %s: %w", ref, pulled, digest, ErrDigestMismatch)
	}
	bundle, err := oap.Unpack(raw)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("unpack %q: %w", ref, err)
	}

	p := Prompt{Ref: ref, Digest: digest, Verified: pinned, Bundle: bundle}
	if !d.Confirm(p) {
		return nil, Prompt{}, fmt.Errorf("%q: %w", ref, ErrCancelled)
	}
	return raw, p, nil
}

// DialogText renders the confirmation dialog. Source, digest and signature
// status come first; bundle-supplied text strictly follows them.
func DialogText(p Prompt) (title, message string) {
	sig := "not verified"
	if p.Verified {
		sig = "verified (OAP agents key)"
	}
	short := strings.TrimPrefix(p.Digest, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Source: %s\nDigest: %s\nSignature: %s\n\n", p.Ref, short, sig)
	if p.Bundle != nil && p.Bundle.Manifest != nil {
		a := p.Bundle.Manifest.Agent
		name := a.DisplayName
		if name == "" {
			name = a.Name
		}
		fmt.Fprintf(&b, "Agent: %s v%s", sanitize(name, maxNameRunes), sanitize(a.Version, maxNameRunes))
		if d := sanitize(a.Description, maxDescRunes); d != "" {
			fmt.Fprintf(&b, "\nDescription: %s", d)
		}
	}
	return "Install agent?", b.String()
}

const (
	maxNameRunes = 80
	maxDescRunes = 300
)

// sanitize flattens bundle-supplied text to one line (control characters and
// newlines become spaces, runs collapse) and caps it at max runes, so it can
// never forge a header line in the dialog.
func sanitize(s string, max int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "\u2026"
	}
	return s
}
