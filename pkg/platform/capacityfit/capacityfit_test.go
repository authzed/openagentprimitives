package capacityfit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

const mib = int64(1024 * 1024)
const gib = 1024 * mib

// desktopCeiling/desktopHeadroom describe a desktop-shaped cluster: a single
// ~3.8Gi node with ~1.9Gi already booked. Shared read-only fixtures across
// tests below (never mutated), so sharing them is safe even under t.Parallel.
var desktopCeiling = schedfit.Ceiling{
	Known: true, CPUMilli: 4000, MemBytes: 3800 * mib, EphemeralBytes: 20 * gib,
	Node: "oap-desktop", Source: "largest node oap-desktop (3.7 GiB memory allocatable)",
}
var desktopHeadroom = schedfit.Headroom{Known: true, CPUMilli: 2000, MemBytes: 1900 * mib, EphemeralBytes: 15 * gib}

// noInstalled is the seam for "this class is not on the cluster yet".
func noInstalled(string) (*unstructured.Unstructured, error) { return nil, nil }

// installedClass builds the *unstructured.Unstructured "live" object the
// installed seam returns — the same shape sandboxClass builds for the
// bundled CR, since Questions treats both identically (classResources reads
// spec.resources off either one the same way). Reusing sandboxClass here is
// deliberate: it keeps the "live" fixture from ever going through a typed
// resource.Quantity field, which is exactly the round-trip C1 forbids.
func installedClass(name, mem string) *unstructured.Unstructured {
	return sandboxClass(name, "", mem, "", "", "")
}

// sandboxClass builds an unstructured SpiceboxClass with the given resources.
// Empty strings omit the field. Uses a made-up fixture name — never an
// example's name.
func sandboxClass(name, cpu, mem, eph, tmp, work string) *unstructured.Unstructured {
	res := map[string]any{}
	for k, v := range map[string]string{"cpu": cpu, "memory": mem, "ephemeralStorage": eph, "tmpSize": tmp, "workSize": work} {
		if v != "" {
			res[k] = v
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SpiceboxClass",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"resources": res},
	}}
}

func dimension(t *testing.T, name string) capacityDimension {
	t.Helper()
	for _, d := range capacityDimensions {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no capacity dimension named %q", name)
	return capacityDimension{}
}

// qstr renders v in dimension d's quantity form. A helper because
// d.Qty(v).String() cannot be written inline — Quantity.String() has a
// pointer receiver and a function-call result is not addressable.
func qstr(d capacityDimension, v int64) string {
	q := d.Qty(v)
	return q.String()
}

func findQuestion(t *testing.T, qs []oap.Question, name string) oap.Question {
	t.Helper()
	for _, q := range qs {
		if q.Name == name {
			return q
		}
	}
	t.Fatalf("no question named %q among %d question(s)", name, len(qs))
	return oap.Question{}
}

// ---- moved unchanged from cmd/oap/internal/agentcmd/install_capacity_test.go ----

func TestClassResourcesAndRequests(t *testing.T) {
	t.Run("string quantities decode, and limits become requests", func(t *testing.T) {
		res, err := classResources(sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi"))
		require.NoError(t, err)

		req := classRequests(res)
		assert.Equal(t, int64(1000), req.Cpu().MilliValue())
		assert.Equal(t, 4*gib, req.Memory().Value())
		eph := req[corev1.ResourceEphemeralStorage] // map index is unaddressable; Value() has a pointer receiver
		assert.Equal(t, gib, eph.Value())
	})

	t.Run("numeric quantities decode too", func(t *testing.T) {
		cr := sandboxClass("test-sandbox", "1", "", "", "", "")
		spec := cr.Object["spec"].(map[string]any)
		spec["resources"].(map[string]any)["memory"] = int64(4294967296)

		res, err := classResources(cr)
		require.NoError(t, err)
		assert.Equal(t, 4*gib, res.Memory.Value())
	})

	t.Run("missing spec.resources is an error the caller can skip on", func(t *testing.T) {
		cr := &unstructured.Unstructured{Object: map[string]any{
			"kind": "SpiceboxClass", "metadata": map[string]any{"name": "test-sandbox"},
		}}
		_, err := classResources(cr)
		assert.Error(t, err)
	})
}

func TestClassFloor(t *testing.T) {
	mem := dimension(t, "memory")
	cpu := dimension(t, "cpu")

	t.Run("memory floor is tmpSize + workSize + margin", func(t *testing.T) {
		res, err := classResources(sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi"))
		require.NoError(t, err)
		assert.Equal(t, 768*mib+memoryFloorMargin, classFloor(mem, res))
	})

	t.Run("unset tmp/work use the API defaults (50Mi + 100Mi)", func(t *testing.T) {
		res, err := classResources(sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "", ""))
		require.NoError(t, err)
		assert.Equal(t, 150*mib+memoryFloorMargin, classFloor(mem, res))
	})

	t.Run("cpu has no derived floor, just the minimum", func(t *testing.T) {
		res, err := classResources(sandboxClass("test-sandbox", "8", "4Gi", "1Gi", "512Mi", "256Mi"))
		require.NoError(t, err)
		assert.Equal(t, cpu.Min, classFloor(cpu, res))
	})
}

func TestSuggestFit(t *testing.T) {
	mem := dimension(t, "memory")
	ceiling := schedfit.Ceiling{Known: true, MemBytes: 3800 * mib, CPUMilli: 4000, Node: "oap-desktop"}

	t.Run("uses free headroom when it is tighter than the ceiling", func(t *testing.T) {
		h := schedfit.Headroom{Known: true, MemBytes: 1900 * mib}
		got := suggestFit(mem, ceiling, h, 1280*mib)
		assert.Equal(t, int64(1792)*mib, got, "1900Mi rounds down to the 256Mi granularity")
		assert.Zero(t, got%mem.Round, "the suggestion must land on the granularity")
	})

	t.Run("falls back to the ceiling when headroom is unknown", func(t *testing.T) {
		got := suggestFit(mem, ceiling, schedfit.Headroom{}, 1280*mib)
		assert.Equal(t, int64(3584)*mib, got, "3800Mi rounds down to 3584Mi")
	})

	t.Run("never returns below the floor", func(t *testing.T) {
		h := schedfit.Headroom{Known: true, MemBytes: 100 * mib}
		got := suggestFit(mem, ceiling, h, 1280*mib)
		assert.Equal(t, 1280*mib, got)
	})

	t.Run("small headroom drift does not move the suggestion (SSA idempotency)", func(t *testing.T) {
		a := suggestFit(mem, ceiling, schedfit.Headroom{Known: true, MemBytes: 1900 * mib}, 1280*mib)
		b := suggestFit(mem, ceiling, schedfit.Headroom{Known: true, MemBytes: 1910 * mib}, 1280*mib)
		assert.Equal(t, a, b, "a 10Mi change must not produce a different applied value")
	})
}

func TestDimensionsCoverEveryCheckedResource(t *testing.T) {
	var names []string
	for _, d := range capacityDimensions {
		names = append(names, d.Name)
		assert.NotZero(t, d.Round, "%s: rounding granularity is required for SSA idempotency", d.Name)
		assert.NotZero(t, d.Min, "%s: a minimum is required", d.Name)
		assert.NotNil(t, d.Unit)
		assert.NotNil(t, d.Qty)
		assert.NotNil(t, d.Ceiling)
		assert.NotNil(t, d.Headroom)
	}
	assert.ElementsMatch(t, []string{"cpu", "memory", "ephemeral-storage"}, names)
}

func TestQuantityRoundTripPerDimension(t *testing.T) {
	for _, d := range capacityDimensions {
		t.Run(d.Name, func(t *testing.T) {
			q := d.Qty(d.Round * 7)
			assert.Equal(t, d.Round*7, d.Unit(q), "Qty and Unit must be inverses")
			assert.NotEmpty(t, q.String())
		})
	}
}

// ---- new: Questions() ----

func TestQuestions_FittingClass_NoQuestion(t *testing.T) {
	cr := sandboxClass("test-sandbox", "500m", "256Mi", "500Mi", "", "")
	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, noInstalled)

	require.NoError(t, err)
	assert.Empty(t, qs, "a class that already fits needs no question")
	assert.Empty(t, notices)
}

func TestQuestions_OversizedClass_QuestionShapeAndValidation(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi")
	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, noInstalled)
	require.NoError(t, err)
	require.Len(t, qs, 1, "only memory exceeds this cluster's ceiling")

	q := qs[0]
	assert.Equal(t, "capacity.test-sandbox.memory", q.Name)
	assert.Equal(t, oap.QString, q.Type)
	assert.Equal(t, []oap.Binding{{Target: "SpiceboxClass/test-sandbox#spec.resources.memory"}}, q.Binding)
	assert.Equal(t, "1792Mi", q.Default, "rounds down to the 256Mi granularity, per suggestFit")
	assert.NotEmpty(t, q.Prompt)
	assert.NotEmpty(t, q.Description)
	assert.False(t, q.Unchanged, "a freshly-computed clamp (nothing installed to adopt) is a new decision, not a no-op")

	// I1: a clamp — even a non-adopted, freshly-computed one — is the single
	// most consequential outcome this package produces and must never be
	// silent. The notice must name the EXACT applied string (q.Default), not a
	// human-readable rendering of it, so it matches what `kubectl get` shows.
	require.Len(t, notices, 1, "lowering memory from 4Gi to 1792Mi must be surfaced, not silent")
	assert.Contains(t, notices[0], "test-sandbox")
	assert.Contains(t, notices[0], "1792Mi", "the notice must cite the exact applied string, not a human-readable rendering of it (e.g. \"1.8 GiB\")")

	// Prove the CEL rule actually accepts/rejects, rather than just asserting
	// the expression string — this exercises the real evaluator (evalValidation
	// is unexported, so we go through install.Resolve, its public entry point;
	// pkg/platform/oap/install does not import pkg/platform/capacityfit, so there is no cycle).
	mem := dimension(t, "memory")
	const floor = 1280 * mib // tmpSize(512Mi) + workSize(256Mi) + memoryFloorMargin(512Mi)
	const ceiling = 3800 * mib

	_, _, err = install.Resolve([]oap.Question{q}, "", nil, false)
	assert.NoError(t, err, "the synthesized default must satisfy its own validation rule")

	_, _, err = install.Resolve([]oap.Question{q}, "", map[string]string{q.Name: qstr(mem, floor-1)}, false)
	assert.Error(t, err, "one unit below the floor must fail validation")

	_, _, err = install.Resolve([]oap.Question{q}, "", map[string]string{q.Name: qstr(mem, ceiling+1)}, false)
	assert.Error(t, err, "one unit above the ceiling must fail validation")

	_, _, err = install.Resolve([]oap.Question{q}, "", map[string]string{q.Name: qstr(mem, floor)}, false)
	assert.NoError(t, err, "the floor itself is inclusive")

	_, _, err = install.Resolve([]oap.Question{q}, "", map[string]string{q.Name: qstr(mem, ceiling)}, false)
	assert.NoError(t, err, "the ceiling itself is inclusive")
}

// A class can fit the node's ALLOCATABLE and still never schedule, because the
// scheduler places pods against what is FREE — allocatable minus the requests
// the control plane has already booked. Gating the clamp question on the
// ceiling alone installs such a class at its bundled size and wedges its pod
// Pending forever ("Insufficient memory") with nothing having warned anyone.
// 2Gi is under desktopCeiling's 3800Mi but over desktopHeadroom's 1900Mi.
func TestQuestions_FitsCeilingButNotHeadroom_QuestionClampsToFree(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "2Gi", "1Gi", "512Mi", "256Mi")
	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, noInstalled)
	require.NoError(t, err)
	require.Len(t, qs, 1, "memory fits the ceiling but not what is actually free, so it must still be asked about")

	q := qs[0]
	assert.Equal(t, "capacity.test-sandbox.memory", q.Name)
	assert.Equal(t, "1792Mi", q.Default, "the suggestion must fit the 1900Mi that is free, rounded down to the 256Mi granularity")

	require.Len(t, notices, 1, "lowering a bundle's declared memory must never be silent")
	assert.Contains(t, notices[0], "test-sandbox")
	assert.Contains(t, notices[0], "1792Mi")
}

// The ceiling, not the volatile headroom, must remain the bound a human may
// answer up to: a cluster that is merely busy right now must not permanently
// forbid the operator from keeping the bundle's declared size.
func TestQuestions_HeadroomGatedQuestion_ValidationStillBoundedByCeiling(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "2Gi", "1Gi", "512Mi", "256Mi")
	qs, _, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, noInstalled)
	require.NoError(t, err)
	require.Len(t, qs, 1)

	mem := dimension(t, "memory")
	_, _, err = install.Resolve(qs, "", map[string]string{qs[0].Name: qstr(mem, 3800*mib)}, false)
	assert.NoError(t, err, "the operator may still answer up to the ceiling, above today's headroom")

	_, _, err = install.Resolve(qs, "", map[string]string{qs[0].Name: qstr(mem, 3800*mib+1)}, false)
	assert.Error(t, err, "but never above the ceiling")
}

// Headroom must never turn a fitting class into a question when it is unknown
// (the elastic-cloud / unreadable-pods case) — the ceiling stays the only gate.
func TestQuestions_UnknownHeadroom_CeilingRemainsTheOnlyGate(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "2Gi", "1Gi", "512Mi", "256Mi")
	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, schedfit.Headroom{}, noInstalled)

	require.NoError(t, err)
	assert.Empty(t, qs, "with headroom unknown, a class under the ceiling must not be clamped on a guess")
	assert.Empty(t, notices)
}

// A class whose memory floor is above what is free can be clamped no further —
// the suggestion comes back AT the floor, still too big to schedule today. The
// notice must not claim it now fits: saying "lowering to 3584Mi to fit <node>"
// when 1900Mi is free is the same silent-success this whole check exists to
// prevent, just one layer up.
func TestQuestions_SuggestionStillAboveHeadroom_NoticeSaysItMayNotSchedule(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "2Gi", "1Gi")
	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, noInstalled)
	require.NoError(t, err)
	require.Len(t, qs, 1)

	assert.Equal(t, "3584Mi", qs[0].Default, "tmpSize(2Gi)+workSize(1Gi)+margin(512Mi) is the floor; nothing lower is offerable")
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "1.9 GiB", "the notice must name what is actually free")
	assert.Contains(t, notices[0], "may still not schedule", "and must not claim the clamped value fits")
}

// A bundle's SpiceboxClasses are provisioned as one AgentSession and their pods
// run AT THE SAME TIME, so what has to fit is their combined footprint. Sizing
// each one against the whole free node in isolation lets a two-class bundle
// over-commit: each clears the check alone, and then the second pod to be
// scheduled finds the first has already taken the room.
//
// This is the desktop wedge exactly: 2162Mi free, a 2Gi class and a 256Mi class.
// Clamping the big one to the full 2162Mi (→2048Mi) leaves 114Mi for a sibling
// that needs 256Mi.
func TestQuestions_SiblingClassesInOneBundle_ClampedToShareTheFreeRoom(t *testing.T) {
	ceiling := schedfit.Ceiling{
		Known: true, CPUMilli: 4000, MemBytes: 3902 * mib, EphemeralBytes: 20 * gib,
		Node: "oap-desktop", Source: "largest node oap-desktop (3.8 GiB memory allocatable)",
	}
	headroom := schedfit.Headroom{Known: true, CPUMilli: 2300, MemBytes: 2162 * mib, EphemeralBytes: 15 * gib}

	big := sandboxClass("test-codelike", "1", "2Gi", "1Gi", "512Mi", "256Mi")
	small := sandboxClass("test-gitlike", "500m", "256Mi", "500Mi", "", "")

	qs, _, err := Questions([]*unstructured.Unstructured{big, small}, ceiling, headroom, noInstalled)
	require.NoError(t, err)

	q := findQuestion(t, qs, "capacity.test-codelike.memory")
	assert.Equal(t, "1792Mi", q.Default,
		"the big class may only claim the room left after its 256Mi sibling, rounded down to the 256Mi granularity")

	for _, got := range qs {
		assert.NotEqual(t, "capacity.test-gitlike.memory", got.Name,
			"the small class already fits alongside its sibling and must be left alone")
	}
}

func TestQuestions_FloorAboveCeiling_HardError(t *testing.T) {
	tiny := schedfit.Ceiling{Known: true, MemBytes: 1 * gib, CPUMilli: 2000, EphemeralBytes: 20 * gib, Node: "tiny", Source: "largest node tiny"}
	cr := sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi")

	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, tiny, schedfit.Headroom{}, noInstalled)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-sandbox")
	assert.Contains(t, err.Error(), "cannot run")
	// Memory's derived floor (tmpSize+workSize memory-backed emptyDirs) is what
	// makes this specific case unfittable — the tmpfs/OOM rationale (FloorWhy on
	// the memory row) must actually ride along, not just the bare numbers.
	assert.Contains(t, err.Error(), "memory-backed", "the memory-specific floor rationale must be present for a memory hard error")
	assert.Nil(t, qs, "no question can help when nothing fits")
	assert.Nil(t, notices)
}

// TestQuestions_FloorAboveCeiling_NonMemoryDimension_NoMemoryOrOOMProse is the
// sibling regression to TestQuestions_FloorAboveCeiling_HardError: cpu and
// ephemeral-storage have no derived floor beyond their bare Min, so their
// hard-error text must NOT borrow memory's tmpfs-backed/OOM rationale (it
// would simply be false for them — cpu below floor is scheduling
// starvation, not an OOM kill). Forces the cpu floor (Min=100m) above a
// tiny 50m ceiling; memory and ephemeral-storage stay comfortably within
// their own ceilings so cpu is the only exceeded dimension.
func TestQuestions_FloorAboveCeiling_NonMemoryDimension_NoMemoryOrOOMProse(t *testing.T) {
	tinyCPU := schedfit.Ceiling{Known: true, CPUMilli: 50, MemBytes: 8 * gib, EphemeralBytes: 20 * gib, Node: "tiny-cpu", Source: "largest node tiny-cpu"}
	cr := sandboxClass("test-sandbox", "200m", "256Mi", "", "", "")

	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, tinyCPU, schedfit.Headroom{}, noInstalled)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-sandbox")
	assert.Contains(t, err.Error(), "cpu")
	assert.NotContains(t, err.Error(), "memory-backed", "cpu's floor rationale must not borrow memory's tmpfs explanation")
	assert.NotContains(t, err.Error(), "OOM", "cpu below floor is scheduling starvation, never an OOM kill")
	assert.Nil(t, qs, "no question can help when nothing fits")
	assert.Nil(t, notices)
}

// TestQuestions_InstalledInRangeValue_DefaultsVerbatim is the regression test
// for the round-trip rule: the default for an already-installed value must be
// the EXACT TEXT still on the live CR (verbatim, not merely value-equal) —
// never anything that has passed through a resource.Quantity's Value()/
// MilliValue() (which rounds) or even through Quantity.String() on a value that
// was never cached verbatim: apimachinery caches a parsed Quantity's input text
// only on an integer fast path, so a fractional "1.8Gi" re-renders as
// "1932735283200m". Parsing the installed field and rendering THAT turns "1.8Gi"
// into "1932735283200m" on re-install though nothing the operator asked for
// changed — an SSA idempotency violation these tests catch by asserting the
// literal, not merely a re-parseable equivalent.
func TestQuestions_InstalledInRangeValue_DefaultsVerbatim(t *testing.T) {
	cases := []struct {
		name string
		// installedMem has no fractional component, so Unit() (which rounds up
		// to a whole byte) loses nothing — the buggy int64 round-trip happens
		// to coincide with the correct answer here. It is included as a sanity
		// check, not as the regression case.
		installedMem                  string
		wantDiffersFromInt64RoundTrip bool
	}{
		{name: "canonical value round-trips (sanity)", installedMem: "1792Mi", wantDiffersFromInt64RoundTrip: false},
		{name: "non-canonical value is preserved literally (regression: no Quantity round-trip at all)", installedMem: "1.8Gi", wantDiffersFromInt64RoundTrip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi")
			installed := func(name string) (*unstructured.Unstructured, error) {
				return installedClass(name, tc.installedMem), nil
			}

			qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, installed)
			require.NoError(t, err)

			q := findQuestion(t, qs, "capacity.test-sandbox.memory")
			assert.Equal(t, tc.installedMem, q.Default, "default must be the exact text still on the live CR")
			assert.True(t, q.Unchanged, "adopting the installed value is a byte-identical SSA no-op, not a new decision")

			mem := dimension(t, "memory")
			buggy := qstr(mem, mem.Unit(resource.MustParse(tc.installedMem))) // what a Unit()->int64->Qty() round-trip would produce
			if tc.wantDiffersFromInt64RoundTrip {
				assert.NotEqual(t, buggy, q.Default, "must not regress to rendering via Unit()->int64->Qty().String()")
			}

			require.Len(t, notices, 1, "adopting the installed value must be surfaced, not silent")
			assert.Contains(t, notices[0], "keeping")
		})
	}
}

// TestQuestions_InstalledValueAsNumber_FallsBackToQuantityString covers C1's
// detail (i): a bare JSON number has no unit suffix to preserve —
// unstructured.NestedString errors on a non-string value, so the only sound
// default is the parsed Quantity's own String(), which for a plain number
// (no cached format, no suffix to derive) is the exact byte count as decimal
// text — never "1792Mi" out of thin air; there is nothing in a bare number
// that says "Mi".
func TestQuestions_InstalledValueAsNumber_FallsBackToQuantityString(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi")
	installed := func(name string) (*unstructured.Unstructured, error) {
		live := installedClass(name, "")
		spec := live.Object["spec"].(map[string]any)
		spec["resources"].(map[string]any)["memory"] = int64(1879048192) // == 1792Mi, authored as a bare number
		return live, nil
	}

	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, installed)
	require.NoError(t, err)

	q := findQuestion(t, qs, "capacity.test-sandbox.memory")
	assert.Equal(t, "1879048192", q.Default, "no unit suffix to preserve, so the raw byte count is the correct (and only) sound rendering")
	assert.True(t, q.Unchanged, "still adopting the installed value, even via the Quantity-string fallback")
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "keeping")
}

func TestQuestions_InstalledOutOfRangeValue_ReResolved(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi")
	installed := func(name string) (*unstructured.Unstructured, error) {
		return installedClass(name, "8Gi"), nil // above the ceiling
	}

	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, installed)
	require.NoError(t, err)

	q := findQuestion(t, qs, "capacity.test-sandbox.memory")
	assert.Equal(t, "1792Mi", q.Default, "an installed value that no longer fits falls back to the suggestion")
	assert.False(t, q.Unchanged, "an out-of-range installed value can't be adopted verbatim — this IS a new decision")

	// I1: nothing was ADOPTED, but a fresh clamp is still being applied, so it
	// must still be surfaced (never silently substituted for the bundle's ask).
	require.Len(t, notices, 1, "the fresh clamp must be surfaced even though nothing was adopted")
	assert.NotContains(t, notices[0], "keeping", "this is a fresh clamp, not an adoption of the installed value")
	assert.Contains(t, notices[0], "1792Mi")
}

func TestQuestions_UnknownCeiling_NoQuestionsOneNotice(t *testing.T) {
	cr := sandboxClass("test-sandbox", "1", "4Gi", "1Gi", "512Mi", "256Mi")
	unknown := schedfit.Ceiling{Source: "GKE node pools autoscale"}

	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, unknown, schedfit.Headroom{}, noInstalled)

	require.NoError(t, err)
	assert.Empty(t, qs)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "GKE node pools autoscale")
}

// TestQuestions_UnknownCeiling_NoSpiceboxClassAtAll_NoNotice is the M4 fix: an
// unknown ceiling (the common case on an elastic cloud) must not warn about a
// check that had nothing to run against — a bundle with no SpiceboxClass at
// all. Warning on every such install trains an operator to skim past
// Result.Warnings, exactly where a real clamp notice would otherwise stand out.
func TestQuestions_UnknownCeiling_NoSpiceboxClassAtAll_NoNotice(t *testing.T) {
	other := &unstructured.Unstructured{Object: map[string]any{
		"kind": "AgentClass", "metadata": map[string]any{"name": "demo-class"},
	}}
	unknown := schedfit.Ceiling{Source: "GKE node pools autoscale"}

	qs, notices, err := Questions([]*unstructured.Unstructured{other}, unknown, schedfit.Headroom{}, noInstalled)

	require.NoError(t, err)
	assert.Empty(t, qs)
	assert.Empty(t, notices, "nothing to check means nothing to warn about, even when the ceiling itself is unknown")
}

func TestQuestions_ZeroCeilingDimension_Skipped(t *testing.T) {
	// memory's ceiling is unknown (0) even though the overall ceiling IS known;
	// ephemeral-storage exceeds, so the class is not simply skipped wholesale.
	c := schedfit.Ceiling{Known: true, CPUMilli: 4000, MemBytes: 0, EphemeralBytes: 20 * gib, Node: "n1", Source: "largest node n1"}
	cr := sandboxClass("test-sandbox", "1", "4Gi", "50Gi", "512Mi", "256Mi")

	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, c, schedfit.Headroom{}, noInstalled)

	require.NoError(t, err)
	require.Len(t, qs, 1, "only ephemeral-storage gets a question; memory's zero ceiling is fail-safe skipped")
	assert.Equal(t, "capacity.test-sandbox.ephemeral-storage", qs[0].Name)

	// M1: the skipped dimension must not be silent — the class DID declare
	// memory: 4Gi, and it goes completely unverified; the ephemeral-storage
	// question alone reads as "everything else is fine," which is false.
	// capacityDimensions iterates cpu/memory/ephemeral-storage in that fixed
	// table order, so notices[0] is the memory skip and notices[1] is the
	// eph clamp (I1) — both required, neither silent.
	require.Len(t, notices, 2, "one notice for the skipped memory dimension, one for the ephemeral-storage clamp actually applied")
	assert.Contains(t, notices[0], "test-sandbox")
	assert.Contains(t, notices[0], "memory")
	assert.Contains(t, notices[0], "could not be checked")
	assert.Contains(t, notices[1], "ephemeral-storage")
}

func TestQuestions_MalformedSpecResources_NoticeNotError(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]any{
		"kind": "SpiceboxClass", "metadata": map[string]any{"name": "test-sandbox"},
		"spec": map[string]any{},
	}}

	qs, notices, err := Questions([]*unstructured.Unstructured{cr}, desktopCeiling, desktopHeadroom, noInstalled)

	require.NoError(t, err, "an unreadable class must not abort the whole install")
	assert.Empty(t, qs)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "test-sandbox")
}

func TestQuestions_NonSpiceboxClassDocsIgnored(t *testing.T) {
	other := &unstructured.Unstructured{Object: map[string]any{
		"kind": "AgentClass", "metadata": map[string]any{"name": "demo-class"},
	}}

	qs, notices, err := Questions([]*unstructured.Unstructured{other}, desktopCeiling, desktopHeadroom, noInstalled)

	require.NoError(t, err)
	assert.Empty(t, qs)
	assert.Empty(t, notices)
}
