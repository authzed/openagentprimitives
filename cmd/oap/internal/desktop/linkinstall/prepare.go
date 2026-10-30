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

	"github.com/Masterminds/semver/v3"
	"oras.land/oras-go/v2/registry"

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
	// ErrDigestMismatch means the pulled content is not the verified/resolved
	// digest, or a ref that names a digest resolved to a different one.
	ErrDigestMismatch = errors.New("pulled digest differs from the verified digest")
	// ErrInvalidSource means a configured install source is malformed, so
	// the allowlist itself cannot be evaluated.
	ErrInvalidSource = errors.New("invalid install source")
	// ErrInvalidRef means the link's ref is not a usable OCI reference.
	ErrInvalidRef = errors.New("invalid OCI reference")
	// ErrTrustedKey means the embedded signing key could not be loaded.
	ErrTrustedKey = errors.New("trusted signing key unavailable")
	// ErrConfirmFailed means the approval dialog could not be shown; nothing
	// was approved, but the user did not decline either.
	ErrConfirmFailed = errors.New("install approval dialog failed")
	// ErrDowngrade means the link's bundle is an older version of an agent
	// already installed. A link never downgrades: a signed old release with a
	// known flaw is still signed, so a rollback has to be deliberate.
	ErrDowngrade = errors.New("link would downgrade an installed agent")
	// ErrInstalledLookup means the installed version could not be read, so
	// the downgrade check could not run.
	ErrInstalledLookup = errors.New("read the installed agent version")
	// ErrUnreadableBundle means the bundle's contents could not be summarized
	// for the approval dialog.
	ErrUnreadableBundle = errors.New("summarize the agent bundle")
)

// Prompt is what the user is asked to approve.
type Prompt struct {
	Ref, Digest string
	Verified    bool
	Bundle      *oap.Bundle
	// Capabilities is what the bundle graph would grant its agents.
	Capabilities oap.Capabilities
	// Installed is the version of this agent already installed; empty when
	// it is not installed (or its version is unknown).
	Installed string
}

// Installed is what Deps.Installed reports about an agent already installed.
type Installed struct {
	// Found is false when no agent of that name is installed.
	Found bool
	// Version is the installed bundle's agent.version; may be empty.
	Version string
}

// Deps are the side-effecting collaborators, injected so Prepare is testable
// and platform-neutral.
type Deps struct {
	Sources []desktop.InstallSource
	Resolve func(ctx context.Context, ref string) (string, error)
	Verify  func(ctx context.Context, ref string, pub crypto.PublicKey) (string, error)
	Pull    func(ctx context.Context, pinnedRef string) ([]byte, string, error)
	// Installed reports the already-installed copy of the bundle's agent, if
	// any. An error refuses the link: the downgrade check could not run.
	Installed func(ctx context.Context, b *oap.Bundle) (Installed, error)
	// Confirm asks the user to approve p. A non-nil error means the dialog
	// itself failed (approved is then ignored); it is not a decline.
	Confirm func(Prompt) (approved bool, err error)
}

// Prepare fetches and gets approval for ref, returning the approved bundle
// bytes. Every refusal wraps a sentinel or the underlying error and names ref.
func Prepare(ctx context.Context, ref string, d Deps) ([]byte, Prompt, error) {
	if d.Resolve == nil || d.Verify == nil || d.Pull == nil || d.Installed == nil || d.Confirm == nil {
		return nil, Prompt{}, fmt.Errorf("install %q: misconfigured dependencies (nil Resolve/Verify/Pull/Installed/Confirm)", ref)
	}
	// Validate every source first, so a remaining AllowedSource error is
	// about the ref and each failure can be reported as what it is.
	for _, src := range d.Sources {
		if err := oci.ValidatePrefix(src.Prefix); err != nil {
			return nil, Prompt{}, fmt.Errorf("install source check for %q: %w: %w", ref, ErrInvalidSource, err)
		}
	}
	_, ok, err := desktop.AllowedSource(d.Sources, ref)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("install source check for %q: %w: %w", ref, ErrInvalidRef, err)
	}
	if !ok {
		return nil, Prompt{}, fmt.Errorf("%q: %w", ref, ErrNotAllowed)
	}

	// Any error from RequiredKey is a refusal, even if it also reported pinned.
	pub, pinned, err := trustedkeys.RequiredKey(ref)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("trusted key lookup for %q: %w: %w", ref, ErrTrustedKey, err)
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

	// A ref that already names a digest must resolve to exactly that digest;
	// otherwise the bytes approved would not be the bytes the link named.
	if named, ok := namedDigest(ref); ok && named != digest {
		return nil, Prompt{}, fmt.Errorf("%q: names %s but resolved to %s: %w", ref, named, digest, ErrDigestMismatch)
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

	if bundle.Manifest == nil {
		return nil, Prompt{}, fmt.Errorf("unpack %q: bundle has no manifest", ref)
	}
	caps, err := oap.SummarizeCapabilities(bundle)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("%q: %w: %w", ref, ErrUnreadableBundle, err)
	}
	agent := bundle.Manifest.Agent
	inst, err := d.Installed(ctx, bundle)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("%q: %w: %w", ref, ErrInstalledLookup, err)
	}
	if inst.Found && isDowngrade(agent.Version, inst.Version) {
		return nil, Prompt{}, fmt.Errorf("%q: %s v%s is older than the installed v%s: %w", ref, agent.Name, agent.Version, inst.Version, ErrDowngrade)
	}

	p := Prompt{Ref: ref, Digest: digest, Verified: pinned, Bundle: bundle, Capabilities: caps}
	if inst.Found {
		p.Installed = inst.Version
	}
	approved, err := d.Confirm(p)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("%q: %w: %w", ref, ErrConfirmFailed, err)
	}
	if !approved {
		return nil, Prompt{}, fmt.Errorf("%q: %w", ref, ErrCancelled)
	}
	return raw, p, nil
}

// isDowngrade reports whether offered is an older version than installed.
// Versions that do not both parse as semver cannot be ordered and are not
// treated as a downgrade; the dialog still shows the installed version.
func isDowngrade(offered, installed string) bool {
	o, err := semver.NewVersion(offered)
	if err != nil {
		return false
	}
	i, err := semver.NewVersion(installed)
	if err != nil {
		return false
	}
	return o.LessThan(i)
}

// namedDigest returns the digest ref is addressed by, if it is addressed by
// one ("host/repo@sha256:…") rather than a tag.
func namedDigest(ref string) (string, bool) {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return "", false
	}
	d, err := parsed.Digest()
	if err != nil {
		return "", false
	}
	return d.String(), true
}

// DialogText renders the confirmation dialog. Source, digest, signature
// status and any installed version come first, then what the agent would be
// granted, then the bundle's own name and description. Every bundle-supplied
// string is flattened by sanitize, so none can forge a line.
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
	fmt.Fprintf(&b, "Source: %s\nDigest: %s\nSignature: %s\n", p.Ref, short, sig)
	if p.Installed != "" {
		fmt.Fprintf(&b, "Replaces installed: v%s\n", sanitize(p.Installed, maxNameRunes))
	}
	b.WriteString("\n")
	writeCapabilities(&b, p.Capabilities)
	b.WriteString("\n")
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

// sanitize flattens bundle-supplied text to one line (every non-printable
// rune becomes a space, runs collapse) and caps it at max runes, so it can
// never forge a header line in the dialog. Non-printable covers more than
// control characters: format runes such as U+202E RIGHT-TO-LEFT OVERRIDE and
// U+200B ZERO WIDTH SPACE could visually reorder or hide text, and Go's %q
// escapes them as \uXXXX, which AppleScript rejects as a syntax error, so the
// dialog would then fail to show at all. unicode.IsPrint is the same set %q
// leaves unescaped.
func sanitize(s string, max int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "\u2026"
	}
	return s
}

const (
	// maxCapabilityItems caps each capability line, so a bundle that bundles
	// fifty MCP servers cannot push the rest of the dialog off screen.
	maxCapabilityItems = 5
	maxCapabilityRunes = 120
)

// writeCapabilities renders c as one bullet line per kind of grant, in a
// fixed order. A bundle that grants nothing says so, so the absence of the
// section is never ambiguous.
func writeCapabilities(b *strings.Builder, c oap.Capabilities) {
	if c.Empty() {
		b.WriteString("This agent bundles no tools, servers, credentials or network access.\n")
		return
	}
	b.WriteString("This agent gets:\n")
	for _, line := range []struct {
		label string
		items []string
	}{
		{"MCP servers", c.MCPServers},
		{"Sidecar toolboxes", c.SidecarToolboxes},
		{"Sandbox tools", c.Toolspecs},
		{"Network access", c.Network},
		{"Credentials", c.Credentials},
		{"Secrets you'll be asked for", c.Secrets},
		{"Shared cluster resources", c.ClusterDeps},
		{"Bundled agents", c.Agents},
	} {
		if len(line.items) == 0 {
			continue
		}
		shown := line.items
		if len(shown) > maxCapabilityItems {
			shown = shown[:maxCapabilityItems]
		}
		parts := make([]string, len(shown))
		for i, item := range shown {
			parts[i] = sanitize(item, maxCapabilityRunes)
		}
		fmt.Fprintf(b, "\u2022 %s: %s", line.label, strings.Join(parts, "; "))
		if more := len(line.items) - len(shown); more > 0 {
			fmt.Fprintf(b, "; and %d more", more)
		}
		b.WriteString("\n")
	}
}
