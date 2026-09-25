package manifests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestOperatorDeploymentStaysSingleReplica is a tripwire, not a preference: it
// is meant to FAIL the day somebody enables HA for the operator, because the
// operator is not HA-safe yet and the ways it breaks are all silent.
//
// The one this test is named for: memory.Local's append-only write path is a
// check-then-write (pre-check Get, then backend.Put) made atomic by a
// PROCESS-LOCAL striped mutex, appendOnlyWriteLocks in
// pkg/memory/appendonlylock.go. Every append-only write in the cluster funnels
// through the single operator process — other components write over the memory
// HTTP API — so one process's lock is today a complete answer. At two replicas
// there are two independent lock arrays, both miss the same pre-check Get, and
// two writers of different content for one entry id both store, the second
// silently overwriting the first. Nothing errors, nothing logs; the damage
// surfaces only as a broken hash chain under `oap audit verify`. The correct
// fix at that point is an atomic backend compare-and-set, not a bigger lock.
//
// It is not the only blocker, and the full list — the ReadWriteOnce PVC two
// pods cannot both mount, per-subsystem leader-election decisions, once-only
// startup steps, in-memory state that would diverge between replicas, and the
// shared provenance publisher identity — is written up in the HA TODO block at
// the top of internal/cmd/operator/main.go. Read that before deleting or
// relaxing this test.
//
// The assertion is against the embedded install bundle rather than
// config/manager/deployment.yaml directly, because the bundle is what
// `oap install` actually applies; TestInstallYAMLMatchesKustomize already fails
// if the two drift.
//
// SCOPE, stated plainly so nobody reads more safety into this than it has: it
// guards the DECLARED replica count in the shipped manifest, not the running
// one. An HPA, a `kubectl scale`, or an operator someone edited in-cluster
// raises the live count without touching this file, and no test can see that.
// This catches the change at the point it enters the repo — which is the point
// at which someone can still be told why — and nothing after that.
func TestOperatorDeploymentStaysSingleReplica(t *testing.T) {
	docs, err := Split(Install)
	require.NoError(t, err, "splitting the embedded install bundle")

	checked := 0
	for _, d := range docs {
		if d.GetKind() != "Deployment" || d.GetName() != "spicebox-operator" {
			continue
		}
		checked++

		raw, found, err := unstructured.NestedFieldNoCopy(d.Object, "spec", "replicas")
		require.NoError(t, err, "reading spec.replicas of the spicebox-operator Deployment")
		require.True(t, found,
			"the spicebox-operator Deployment must set spec.replicas EXPLICITLY: omitted means Kubernetes "+
				"defaults it to 1, which is the right number but records nothing — this tripwire would have "+
				"no field to read, and config/manager/deployment.yaml would not say anywhere that one replica "+
				"is a correctness requirement rather than a default nobody revisited")

		// Split decodes the bundle through kubeyaml, which routes YAML via JSON,
		// so an integer arrives as float64 rather than the int64
		// unstructured.NestedInt64 insists on. Accept either, so the tripwire
		// keeps working if the splitter's number handling ever changes.
		var replicas int64
		switch v := raw.(type) {
		case int64:
			replicas = v
		case float64:
			replicas = int64(v)
		default:
			require.Failf(t, "spec.replicas is not a number",
				"spec.replicas of the spicebox-operator Deployment decoded as %T (%v), which no comparison "+
					"below can interpret; the tripwire would silently stop guarding", raw, raw)
		}

		assert.EqualValues(t, 1, replicas,
			"the spicebox-operator Deployment is at replicas: %d. The operator is NOT HA-safe: "+
				"memory.Local's append-only write-once guarantee rests on a process-local lock "+
				"(appendOnlyWriteLocks, pkg/memory/appendonlylock.go) that protects nothing across two "+
				"processes, and a second replica reopens the silent overwrite it closes. Raising this is a "+
				"project, not a config edit — see the HA TODO block at the top of "+
				"internal/cmd/operator/main.go for everything else that has to change first, and "+
				"config/manager/deployment.yaml (run `mage manifests` after editing it).",
			replicas)
	}

	require.Equal(t, 1, checked,
		"expected exactly one Deployment named spicebox-operator in the install bundle, found %d; "+
			"if the operator was renamed or split, this tripwire has to follow it", checked)
}
