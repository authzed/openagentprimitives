// Package hook builds the install.InstallOpts.ExtraQuestions closure every
// .oap install surface (the CLI, the macOS desktop installer, admind) wires
// in unchanged: resolve this cluster's scheduling ceiling once, then hand
// pkg/platform/capacityfit.Questions the CRs it is asked about plus a seam to read
// back an already-installed SpiceboxClass.
//
// It is a sibling of pkg/platform/capacityfit rather than part of it because
// that package is deliberately pure (no I/O, no clients — see its package doc)
// and this one's whole job is the I/O it refuses to own: talking to
// cloud.Strategy and the cluster. A separate package keeps that boundary real.
package hook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/capacityfit"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// spiceboxClassGVK is set on every "installed" Get target below — as
// *unstructured.Unstructured, never the typed v1alpha1.SpiceboxClass. A typed
// Get round-trips a quantity like "1.8Gi" through resource.Quantity and loses
// the text it was authored in (String() renders it back as
// "1932735283200m"), which would rewrite the CR's value on every subsequent
// install even though the numeric value never changed — exactly the
// byte-identical-re-apply violation this design avoids. See
// pkg/platform/capacityfit's installedDefault doc comment for the full rationale.
var spiceboxClassGVK = v1alpha1.SchemeGroupVersion.WithKind("SpiceboxClass")

// ExtraQuestions matches install.InstallOpts.ExtraQuestions' (unnamed) field
// type exactly — named here purely so this package's exported signatures read
// as a type instead of a five-line inline func.
type ExtraQuestions func(ctx context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error)

// New resolves this cluster's scheduling ceiling once (via cloud.Detect +
// cloud.Strategy.SchedulingCeiling over typed/ctrl) and returns an
// ExtraQuestions hook over it. Every failure path — no typed/ctrl client, no
// cloud.Strategy registered, cloud detection, the ceiling read itself —
// yields a hook that emits no questions and one notice explaining why: a
// capacity read that cannot run must never block an install (the operator's
// runtime fast-fail still covers a pod that genuinely cannot be placed).
func New(ctx context.Context, typed kubernetes.Interface, ctrl client.Client) ExtraQuestions {
	skip := func(notice string) ExtraQuestions {
		return func(context.Context, []*unstructured.Unstructured) ([]oap.Question, []string, error) {
			return nil, []string{notice}, nil
		}
	}

	// A caller with nothing to read from (admind's Config.Clientset unset) is
	// exactly the same "cannot run this check" case as every failure below —
	// one guard, one notice, rather than every caller re-deriving its own.
	if typed == nil || ctrl == nil {
		return skip("capacity check skipped: no cluster clientset configured")
	}

	strat, err := cloud.Detect(ctx, typed)
	if err != nil {
		// Detect only ever errors on the same node-list call SchedulingCeiling
		// itself makes below — fall back to the default kind rather than skip
		// outright, giving its own fail-safe SchedulingCeiling the same shot at
		// answering (it reports the identical "could not list nodes" notice if
		// the API is genuinely unreachable, rather than a separate one here).
		//
		// Default() errors only when no kind is registered at all: a binary
		// missing its blank import (cmd/oap/cloudimports.go,
		// internal/cmd/operator/cloudimports.go), not a per-request condition.
		strat, err = cloud.Default()
		if err != nil {
			return skip("capacity check skipped: " + err.Error())
		}
	}

	ceiling, headroom, err := strat.SchedulingCeiling(ctx, cloud.Clients{Typed: typed, Ctrl: ctrl})
	if err != nil {
		return skip(fmt.Sprintf("capacity check skipped: could not read this cluster's capacity (%v)", err))
	}

	// installed reads the live SpiceboxClass by name, cluster-scoped (no
	// namespace on the ObjectKey — SpiceboxClass has no namespace to set).
	// Absent -> (nil, nil): capacityfit reads that as "nothing installed yet,"
	// not an error.
	installed := func(name string) (*unstructured.Unstructured, error) {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(spiceboxClassGVK)
		if getErr := ctrl.Get(ctx, client.ObjectKey{Name: name}, live); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return nil, nil
			}
			return nil, getErr
		}
		return live, nil
	}

	// ceiling.Known == false (an elastic cloud, or a node-list read failure) is
	// handled by capacityfit.Questions itself — it returns one explanatory
	// notice and no questions before ever calling installed — so no duplicate
	// handling belongs here.
	return func(_ context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
		return capacityfit.Questions(crs, ceiling, headroom, installed)
	}
}
