// Package resolve turns a class's toolchain names into self-contained,
// frozen mounts. It is a pure function of the class: given the same names
// and the same cluster-scoped SpiceboxToolchain CRs, it always returns the
// same mounts and set digest. That purity is what lets more than one
// controller call it — the session controller (to freeze
// status.ResolvedToolchains at bind time) and, later, the class controller
// (to pre-resolve the overlay a warm pool needs) — without either importing
// the other.
package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolchain"
)

// ErrToolchainMissing and ErrToolchainNotValid are sentinels Resolve wraps
// with %w so the caller can classify the failure into a specific Ready=False
// reason (ReasonToolchainMissing / ReasonToolchainNotValid) via
// stderrors.Is. Any other error (a transient apiserver Get failure, for
// instance) is returned unwrapped by neither sentinel, so the caller
// propagates it instead of persisting a misleading fail-closed condition —
// see the spiceboxsession controller's tcErr handling.
var (
	ErrToolchainMissing  = errors.New("toolchain missing")
	ErrToolchainNotValid = errors.New("toolchain not valid")
)

// Resolve turns a class's toolchain names into self-contained mounts.
// Fails closed on a missing or Valid=False toolchain: a session whose class asks
// for `go` must not quietly start without a compiler.
//
// Names are sorted and deduplicated so the result — and therefore the frozen
// status, the set digest, and the PodSpec — is a pure function of the class.
func Resolve(ctx context.Context, c client.Client, names []string) ([]spiceboxv1alpha1.ToolchainMount, string, error) {
	uniq := make([]string, 0, len(names))
	seen := map[string]struct{}{}
	for _, n := range names {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		uniq = append(uniq, n)
	}
	sort.Strings(uniq)

	out := make([]spiceboxv1alpha1.ToolchainMount, 0, len(uniq))
	for _, n := range uniq {
		var tc spiceboxv1alpha1.SpiceboxToolchain
		if err := c.Get(ctx, client.ObjectKey{Name: n}, &tc); err != nil {
			if k8serrors.IsNotFound(err) {
				return nil, "", fmt.Errorf("%w: toolchain %q: %v", ErrToolchainMissing, n, err)
			}
			// A non-NotFound Get error (apiserver hiccup, timeout, ...) is transient,
			// not "toolchain missing" — return it unwrapped so the caller retries
			// with backoff instead of persisting a misleading ToolchainMissing status.
			return nil, "", fmt.Errorf("get toolchain %q: %w", n, err)
		}
		if !conditions.IsTrue(tc.Status.Conditions, spiceboxv1alpha1.SpiceboxToolchainConditionValid) {
			return nil, "", fmt.Errorf("%w: toolchain %q is not Valid=True", ErrToolchainNotValid, n)
		}
		m, err := spiceboxtoolchain.ToMount(&tc)
		if err != nil {
			return nil, "", fmt.Errorf("toolchain %q: %w", n, err)
		}
		out = append(out, m)
	}
	return out, toolchainSetDigest(out), nil
}

// toolchainSetDigest is a stable hex sha256 over (name, image) pairs, sorted by
// name. Order-independent so a reshuffled class does not produce a new digest;
// image-sensitive so a catalog re-pin does.
func toolchainSetDigest(mounts []spiceboxv1alpha1.ToolchainMount) string {
	sorted := make([]spiceboxv1alpha1.ToolchainMount, len(mounts))
	copy(sorted, mounts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	for _, m := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00", m.Name, m.Image)
	}
	return hex.EncodeToString(h.Sum(nil))
}
