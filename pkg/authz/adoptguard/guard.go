// Package adoptguard gates operator reads of guarded object types (Secret,
// ConfigMap) to objects the operator legitimately manages — adopted via a CR
// reference (carrying AdoptedLabel, so present in the label-filtered cache), or
// on a small fixed-infra allowlist. Any other read is a programming error: in
// Panic mode it panics (loud, crash-looping, audit-visible) so overreach is
// never silent. We trust the operator binary; this makes misuse fatal.
package adoptguard

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AdoptedLabel marks an object the operator has adopted (a CR references it).
// The manager cache runs a label-filtered watch on this, so the cache only
// holds adopted objects.
const AdoptedLabel = "agentprimitives.authzed.com/adopted"

type Mode int

const (
	Panic Mode = iota // default: a non-adopted/non-allowlisted read panics
	Warn              // bring-up only: log loudly + return
)

// Guard is a guarded reader for one object type T.
type Guard[T client.Object] struct {
	// Reader is the LIVE reader (the manager's uncached APIReader). All guarded
	// reads go through it, so contents are never bulk-cached and a just-adopted
	// object's label is visible immediately (no informer lag).
	Reader client.Reader
	// Cache is reserved (the label-filtered manager cache governs WATCHES, not
	// this read path); kept for wiring symmetry. Reads do not use it.
	Cache       client.Reader
	Allowlisted func(types.NamespacedName) bool
	Mode        Mode
	New         func() T
}

// Get retrieves the object identified by nn through a LIVE read, so it can
// distinguish "does not exist" (a benign NotFound, returned as-is) from "exists
// but the operator does not own it" (an overreach). An allowlisted or adopted
// (AdoptedLabel-bearing) object is returned. An object that EXISTS but is
// neither adopted nor allowlisted is refused: Panic mode panics (loud,
// audit-visible), Warn mode logs + returns (bring-up aid). A NotFound is never a
// refusal — absence is not overreach.
func (g *Guard[T]) Get(ctx context.Context, nn types.NamespacedName) (T, error) {
	obj := g.New()
	if err := g.Reader.Get(ctx, nn, obj); err != nil {
		return obj, err // includes NotFound: the object doesn't exist — benign
	}
	if g.Allowlisted(nn) || hasAdoptedLabel(obj) {
		return obj, nil
	}
	// The object EXISTS but is neither adopted nor allowlisted — overreach.
	msg := fmt.Sprintf(
		"adoptguard: refused read of non-adopted, non-allowlisted object %s/%s"+
			" — adopt it via a CR reference or add it to the fixed-infra allowlist",
		nn.Namespace, nn.Name,
	)
	if g.Mode == Warn {
		log.FromContext(ctx).Error(nil, msg)
		return obj, nil
	}
	panic(msg)
}

func hasAdoptedLabel(o client.Object) bool {
	_, ok := o.GetLabels()[AdoptedLabel]
	return ok
}

// WithAdoptedLabel stamps the adoption label on an object the operator creates,
// so operator-minted objects are adopted-by-self (pass the guard + enter the
// label-filtered cache).
func WithAdoptedLabel(o client.Object) {
	l := o.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[AdoptedLabel] = "true"
	o.SetLabels(l)
}
