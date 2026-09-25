// Package capacityfit answers "this SpiceboxClass requests more of some
// resource than this cluster's largest node has — what install-time question
// should ask the operator to lower it?"
//
// It is deliberately pure: no io, no prompting, no CR mutation. Questions
// synthesizes oap.Question values whose Binding overlays the chosen answer
// back onto the CR (see pkg/platform/oap/overlay.go), so clamping a class's resources
// is just an ordinary answered question like any other — the same code path
// serves `oap agent install`, the macOS desktop installer, and the admin UI,
// instead of each carrying its own bespoke prompt-and-clamp policy.
package capacityfit

import (
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

const (
	mebibyte = int64(1024 * 1024)

	// memoryFloorMargin is the room an agent process needs ABOVE its
	// memory-backed scratch. /tmp and /work are tmpfs charged against the same
	// limit (pkg/platform/podspec/builder.go), so a class clamped to exactly
	// tmpSize+workSize has nothing left to run in and is OOM-killed the moment a
	// tool allocates.
	memoryFloorMargin = 512 * mebibyte
)

// capacityDimension is one clampable SpiceboxClass resource. Keeping the three
// as a table rather than three parallel switch statements means the arithmetic
// below never has to care whether it is holding millicores or bytes — and a
// fourth checked resource is a new row, not a new branch in every function.
type capacityDimension struct {
	// Name is user-facing ("ephemeral-storage"); Field is the spec key
	// ("ephemeralStorage"). They differ, so both are recorded.
	Name  string
	Field string
	Res   corev1.ResourceName

	// Round is the granularity a suggested value is rounded DOWN to. Coarse on
	// purpose: it keeps ordinary headroom drift from changing the value install
	// applies, which is what makes a re-install a byte-identical SSA no-op.
	Round int64
	// Min is the hard minimum for dimensions with no derived floor.
	Min int64

	// FloorWhy explains WHY this dimension's floor can be above Min, folded
	// into both the question's Description and the floor-above-ceiling hard
	// error. Empty for cpu and ephemeral-storage: their floor IS Min, with no
	// story beyond the bare number, so no aside is added — memory's derived
	// floor (tmpSize+workSize memory-backed emptyDirs, see classFloor) is the
	// only dimension with something true to say here. Keeping this a
	// per-dimension row (not a `d.Res == corev1.ResourceMemory` branch at each
	// call site) is what stops that rationale from leaking onto cpu/
	// ephemeral-storage messages, where it would simply be false.
	FloorWhy string

	// Unit projects a Quantity into this dimension's integer unit (millicores
	// for cpu, bytes for the rest); Qty is its inverse.
	Unit func(resource.Quantity) int64
	Qty  func(int64) resource.Quantity

	Ceiling  func(schedfit.Ceiling) int64
	Headroom func(schedfit.Headroom) int64
	Human    func(int64) string

	// SetCeiling writes this dimension back onto a Ceiling, the inverse of
	// Ceiling. It exists so fitCeiling can lower each dimension generically
	// instead of switching on Res — same reason the rest of this row is a
	// table: a fourth checked resource stays a new row, not a new branch.
	SetCeiling func(*schedfit.Ceiling, int64)

	// SetHeadroom writes this dimension back onto a Headroom, the inverse of
	// Headroom. Used by allocateRoom to narrow the free room down to one
	// class's share of it — again generically, not per-resource.
	SetHeadroom func(*schedfit.Headroom, int64)
}

var capacityDimensions = []capacityDimension{
	{
		Name: "cpu", Field: "cpu", Res: corev1.ResourceCPU,
		Round: 100, Min: 100, // millicores
		Unit:        func(q resource.Quantity) int64 { return q.MilliValue() },
		Qty:         func(v int64) resource.Quantity { return *resource.NewMilliQuantity(v, resource.DecimalSI) },
		Ceiling:     func(c schedfit.Ceiling) int64 { return c.CPUMilli },
		Headroom:    func(h schedfit.Headroom) int64 { return h.CPUMilli },
		Human:       func(v int64) string { return fmt.Sprintf("%dm", v) },
		SetCeiling:  func(c *schedfit.Ceiling, v int64) { c.CPUMilli = v },
		SetHeadroom: func(h *schedfit.Headroom, v int64) { h.CPUMilli = v },
	},
	{
		Name: "memory", Field: "memory", Res: corev1.ResourceMemory,
		Round: 256 * mebibyte, Min: 256 * mebibyte,
		FloorWhy:    "tmpSize and workSize are memory-backed and charged against this limit — lowering it further would trade a Pending pod for an OOM kill mid-run",
		Unit:        func(q resource.Quantity) int64 { return q.Value() },
		Qty:         func(v int64) resource.Quantity { return *resource.NewQuantity(v, resource.BinarySI) },
		Ceiling:     func(c schedfit.Ceiling) int64 { return c.MemBytes },
		Headroom:    func(h schedfit.Headroom) int64 { return h.MemBytes },
		Human:       schedfit.HumanBytes,
		SetCeiling:  func(c *schedfit.Ceiling, v int64) { c.MemBytes = v },
		SetHeadroom: func(h *schedfit.Headroom, v int64) { h.MemBytes = v },
	},
	{
		Name: "ephemeral-storage", Field: "ephemeralStorage", Res: corev1.ResourceEphemeralStorage,
		Round: 256 * mebibyte, Min: 256 * mebibyte,
		Unit:        func(q resource.Quantity) int64 { return q.Value() },
		Qty:         func(v int64) resource.Quantity { return *resource.NewQuantity(v, resource.BinarySI) },
		Ceiling:     func(c schedfit.Ceiling) int64 { return c.EphemeralBytes },
		Headroom:    func(h schedfit.Headroom) int64 { return h.EphemeralBytes },
		Human:       schedfit.HumanBytes,
		SetCeiling:  func(c *schedfit.Ceiling, v int64) { c.EphemeralBytes = v },
		SetHeadroom: func(h *schedfit.Headroom, v int64) { h.EphemeralBytes = v },
	},
}

// classResources decodes a bundled SpiceboxClass's spec.resources into the
// typed struct, so quantity parsing and the tmp/work defaults come from the API
// types rather than being re-derived here.
//
// It goes through JSON rather than runtime.DefaultUnstructuredConverter because
// a quantity in an unstructured doc can be a string ("4Gi") or a number, and
// resource.Quantity's UnmarshalJSON is what handles both.
func classResources(cr *unstructured.Unstructured) (v1alpha1.SpiceboxResources, error) {
	var res v1alpha1.SpiceboxResources
	raw, found, err := unstructured.NestedMap(cr.Object, "spec", "resources")
	if err != nil {
		return res, fmt.Errorf("spec.resources is malformed: %w", err)
	}
	if !found {
		return res, fmt.Errorf("spec.resources is not set")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return res, fmt.Errorf("re-encode spec.resources: %w", err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return res, fmt.Errorf("decode spec.resources: %w", err)
	}
	return res, nil
}

// classRequests renders a class's declared resources as the request list the
// scheduler will actually see.
//
// SpiceboxClass resources become container LIMITS with no requests set
// (pkg/platform/podspec/builder.go:103-108), and Kubernetes defaults requests to limits
// in exactly that case — so the declared limit IS the request the scheduler
// must find room for. A zero quantity means "not declared" and is omitted.
func classRequests(res v1alpha1.SpiceboxResources) corev1.ResourceList {
	rl := corev1.ResourceList{}
	if !res.CPU.IsZero() {
		rl[corev1.ResourceCPU] = res.CPU
	}
	if !res.Memory.IsZero() {
		rl[corev1.ResourceMemory] = res.Memory
	}
	if !res.EphemeralStorage.IsZero() {
		rl[corev1.ResourceEphemeralStorage] = res.EphemeralStorage
	}
	return rl
}

// classFloor returns the smallest value this class may be clamped to for d.
//
// Memory has a DERIVED floor: /tmp and /work are memory-backed emptyDirs whose
// pages are charged against this same limit, so clamping below
// tmpSize+workSize+margin trades "Pending forever" for "OOM-killed mid-build" —
// a strictly worse failure, because it happens later and reads as a crash
// rather than a capacity problem. The other dimensions have no such coupling
// and just use their minimum.
func classFloor(d capacityDimension, res v1alpha1.SpiceboxResources) int64 {
	if d.Res != corev1.ResourceMemory {
		return d.Min
	}
	tmp := res.EffectiveTmpSize()
	work := res.EffectiveWorkSize()
	floor := tmp.Value() + work.Value() + memoryFloorMargin
	if floor < d.Min {
		return d.Min
	}
	return floor
}

// fitCeiling is the bound a class must clear to be left ALONE — each dimension
// lowered to that class's share of what is actually free on the ceiling node,
// when that is known (see allocateRoom for how the share is decided).
//
// The scheduler places a pod against allocatable MINUS the requests already
// booked on the node, so the raw ceiling answers the wrong question: a class
// under allocatable but over what is free installs at its bundled size and then
// sits Pending forever with "Insufficient memory". On a fixed-size cluster (a
// desktop VM, kind, bare metal) the control plane's own requests are not going
// anywhere, so "free" is the honest bound.
//
// This does NOT weaken schedfit.Headroom's contract that headroom never decides
// something is impossible — it decides only whether to ASK. The hard
// floor-above-ceiling error and the [floor, ceiling] bounds a human may answer
// within both keep using the real ceiling, so a merely-busy cluster can still
// be answered with the bundle's declared value, and an elastic cluster (where
// Ceiling.Known is false and Questions returns early) is untouched.
func fitCeiling(c schedfit.Ceiling, room schedfit.Headroom) schedfit.Ceiling {
	if !room.Known {
		return c
	}
	fit := c
	for _, d := range capacityDimensions {
		if v := d.Headroom(room); v < d.Ceiling(c) {
			d.SetCeiling(&fit, v)
		}
	}
	return fit
}

// allocateRoom splits what is free on the node among the SpiceboxClasses being
// installed together, returning each one's share by class name.
//
// The classes in a bundle are not independent tenants of the node: an
// AgentSession provisions ALL of them and their pods run at once, so what has
// to fit is their combined footprint. Sizing each against the whole free node
// in isolation lets a two-class bundle over-commit — each clears the check
// alone, then the second pod scheduled finds the first has taken the room. A
// 2Gi class and a 256Mi class are each "fine" against 2162Mi free and together
// are not.
//
// Allocation is smallest-first, per dimension independently: a class fitting
// the room still unclaimed reserves exactly what it asked for, and only once
// the remainder runs out does a class get clamped. That keeps the clamp on the
// classes responsible for the overflow instead of shrinking every class a
// little, and it is deterministic (size, then name) — so a re-install of the
// same bundle against the same cluster proposes the same values and stays a
// byte-identical apply.
//
// Returns nil when headroom is unknown; callers fall back to h itself, whose
// Known=false makes fitCeiling and suggestFit ignore it.
func allocateRoom(crs []*unstructured.Unstructured, h schedfit.Headroom) map[string]schedfit.Headroom {
	if !h.Known {
		return nil
	}
	type claim struct {
		name     string
		declared int64
	}

	rooms := map[string]schedfit.Headroom{}
	reqs := map[string]corev1.ResourceList{}
	for _, cr := range crs {
		if cr.GetKind() != "SpiceboxClass" {
			continue
		}
		res, err := classResources(cr)
		if err != nil {
			// Unreadable here is unreadable in Questions too, where it already
			// earns a notice; contributing nothing is the fail-safe reading.
			continue
		}
		rooms[cr.GetName()] = h
		reqs[cr.GetName()] = classRequests(res)
	}

	for _, d := range capacityDimensions {
		claims := make([]claim, 0, len(reqs))
		for name, rl := range reqs {
			if q, ok := rl[d.Res]; ok {
				claims = append(claims, claim{name: name, declared: d.Unit(q)})
			}
		}
		sort.Slice(claims, func(i, j int) bool {
			if claims[i].declared != claims[j].declared {
				return claims[i].declared < claims[j].declared
			}
			return claims[i].name < claims[j].name
		})

		remaining := d.Headroom(h)
		for _, cl := range claims {
			room := rooms[cl.name]
			d.SetHeadroom(&room, remaining)
			rooms[cl.name] = room

			take := cl.declared
			if take > remaining {
				take = remaining
			}
			remaining -= take
		}
	}
	return rooms
}

// roomFor returns the named class's share of the free node, falling back to h
// when there is no allocation (headroom unknown, or a class whose resources were
// unreadable).
func roomFor(rooms map[string]schedfit.Headroom, name string, h schedfit.Headroom) schedfit.Headroom {
	if room, ok := rooms[name]; ok {
		return room
	}
	return h
}

// suggestFit picks the value to offer the user for d: the most that fits right
// now, rounded DOWN to d.Round, and never below floor.
//
// Headroom (what is free on the ceiling node this instant) is what makes the
// suggestion actually schedulable — the ceiling alone would suggest a value the
// control plane's own pods have already booked. When headroom is unknown it
// falls back to the ceiling; the caller warns that such a value may still not
// fit today's load.
func suggestFit(d capacityDimension, c schedfit.Ceiling, h schedfit.Headroom, floor int64) int64 {
	avail := d.Ceiling(c)
	if h.Known && d.Headroom(h) < avail {
		avail = d.Headroom(h)
	}
	v := (avail / d.Round) * d.Round
	if v < floor {
		return floor
	}
	return v
}

// Questions synthesizes one install-time question per (SpiceboxClass,
// dimension) that the cluster's ceiling c cannot schedule as bundled, so
// oap.Apply's binding can clamp it from an ordinary answer instead of a
// bespoke policy mutating the CR directly.
//
// installed reads the SpiceboxClass already on the cluster under a given name
// (nil, nil when absent, or when the caller has no such lookup — e.g. the admin
// UI installing into a fresh namespace). It returns *unstructured.Unstructured,
// never a typed *v1alpha1.SpiceboxClass: a typed Get discards the text a field
// was written in (a Quantity remembers the value, not "1.8Gi" vs
// "1932735283200m"), and never losing that text is this package's whole point —
// see installedDefault. It is a seam, not a client, so every decision here is
// testable without one.
//
// The second return is human-readable notices the caller MUST surface: a
// skipped class/dimension (unreadable spec.resources, an unreadable installed
// class, an unknown per-dimension ceiling) or a clamp/adoption decision. These
// are never silent and never errors — the install can still proceed. The only
// hard error is a class that cannot fit at all (floor > ceiling): no value both
// fits the node and avoids an OOM kill, so no question can help.
func Questions(crs []*unstructured.Unstructured, c schedfit.Ceiling, h schedfit.Headroom,
	installed func(name string) (*unstructured.Unstructured, error)) ([]oap.Question, []string, error) {

	if !c.Known {
		// Nothing to check if the bundle carries no SpiceboxClass at all — an
		// unknown ceiling is the common case on an elastic cloud (GKE Autopilot,
		// EKS/AKS node-group autoscaling), and every install of a bundle with no
		// SpiceboxClass would otherwise print a warning about a check that had
		// nothing to run against. That trains an operator to skim past
		// Result.Warnings, which is exactly where a REAL clamp notice lives.
		hasSpiceboxClass := false
		for _, cr := range crs {
			if cr.GetKind() == "SpiceboxClass" {
				hasSpiceboxClass = true
				break
			}
		}
		if !hasSpiceboxClass {
			return nil, nil, nil
		}
		// The caller already knows why (c.Source explains it); nothing is
		// provable here, so there is nothing to ask and nothing to clamp.
		return nil, []string{fmt.Sprintf("capacity check skipped: %s", c.Source)}, nil
	}

	var qs []oap.Question
	var notices []string

	// What a class must clear to be left alone is its SHARE of what is free,
	// not what the node could offer if it were empty — see fitCeiling and
	// allocateRoom. c itself stays the bound for the hard floor error and the
	// answerable range below.
	rooms := allocateRoom(crs, h)

	for _, cr := range crs {
		if cr.GetKind() != "SpiceboxClass" {
			continue
		}
		name := cr.GetName()

		res, err := classResources(cr)
		if err != nil {
			// Never silent: a class we cannot read is a class we cannot vouch
			// for, but that alone must not abort an otherwise-good install.
			notices = append(notices, fmt.Sprintf("skipping the capacity check for SpiceboxClass %s: %v", name, err))
			continue
		}
		reqs := classRequests(res)
		room := roomFor(rooms, name, h)
		fit := fitCeiling(c, room)
		if _, exceeded := schedfit.Exceeds("SpiceboxClass "+name, reqs, fit); !exceeded {
			continue // already fits every dimension's free room; nothing to ask
		}

		// Read the installed class once per class, not once per dimension. A
		// read failure only costs the idempotency shortcut below, not the class.
		var live *unstructured.Unstructured
		if installed != nil {
			got, err := installed(name)
			if err != nil {
				notices = append(notices, fmt.Sprintf("could not read the installed SpiceboxClass %s (%v); resolving resources from scratch", name, err))
			} else {
				live = got
			}
		}

		for _, d := range capacityDimensions {
			ceiling := d.Ceiling(c)
			declared, ok := reqs[d.Res]
			if ceiling == 0 {
				if ok {
					// The class DID declare a value for this dimension, so silence here
					// would read as "verified fine" when it is really "never checked".
					notices = append(notices, fmt.Sprintf("SpiceboxClass %s: %s could not be checked against this cluster (its %s ceiling is unknown)", name, d.Name, d.Name))
				}
				continue // dimension unknown; fail-safe, same rule as schedfit.Exceeds
			}
			if !ok || d.Unit(declared) <= d.Ceiling(fit) {
				continue // this class already fits this dimension's free room
			}

			floor := classFloor(d, res)
			if floor > ceiling {
				// d.FloorWhy is empty for cpu/ephemeral-storage (their floor is
				// just Min — nothing more to say) and only carries prose for
				// memory (see the memory row's FloorWhy) — so this reads as a bare
				// "needs at least X, only Y is offered" for the other two
				// dimensions, with no borrowed tmpfs/OOM claim that would be false
				// for them.
				why := ""
				if d.FloorWhy != "" {
					why = fmt.Sprintf(" (%s)", d.FloorWhy)
				}
				return nil, nil, fmt.Errorf(
					"this cluster cannot run SpiceboxClass %s: it needs at least %s of %s%s, but %s offers only %s",
					name, d.Human(floor), d.Name, why, c.Source, d.Human(ceiling))
			}

			// capacityQuestion's notice is always non-empty: lowering a bundle's
			// declared resources is the single most consequential outcome this
			// package produces and must never be silent (I1).
			q, notice := capacityQuestion(name, d, c, room, declared, floor, ceiling, live)
			qs = append(qs, q)
			notices = append(notices, notice)
		}
	}

	return qs, notices, nil
}

// capacityQuestion builds the question that clamps one class's one dimension,
// and the notice explaining where its default came from — always non-empty,
// since a clamp lowering a bundle's declared resources is the single most
// consequential outcome this package produces and must never be silent.
func capacityQuestion(className string, d capacityDimension, c schedfit.Ceiling, room schedfit.Headroom,
	declared resource.Quantity, floor, ceiling int64, live *unstructured.Unstructured) (oap.Question, string) {

	name := fmt.Sprintf("%s%s.%s", oap.ReservedQuestionPrefix, className, d.Name)

	def, notice, unchanged := capacityDefault(className, d, c, room, declared, floor, ceiling, live)

	// The Validation bounds are OUR OWN int64s (floor, ceiling), never a value
	// read from the user or the cluster, so rendering them via d.Qty(...).String()
	// is exactly right — there is no round-trip to preserve.
	floorQty, ceilingQty := d.Qty(floor), d.Qty(ceiling)
	floorStr, ceilingStr := floorQty.String(), ceilingQty.String()

	desc := fmt.Sprintf("This class needs at least %s of %s.", d.Human(floor), d.Name)
	if d.FloorWhy != "" {
		desc = fmt.Sprintf("This class needs at least %s: %s.", d.Human(floor), d.FloorWhy)
	}
	if room.Known {
		desc += fmt.Sprintf(" %s of %s is free on %s right now.", d.Human(d.Headroom(room)), d.Name, c.Node)
	}

	q := oap.Question{
		Name: name,
		Type: oap.QString,
		Prompt: fmt.Sprintf("SpiceboxClass %s requests %s=%s but %s offers only %s — lower it to fit?",
			className, d.Name, declared.String(), c.Source, d.Human(ceiling)),
		Description: desc,
		Default:     def,
		Validation: fmt.Sprintf("quantity(args['%s']) >= quantity('%s') && quantity(args['%s']) <= quantity('%s')",
			name, floorStr, name, ceilingStr),
		Binding:   []oap.Binding{{Target: fmt.Sprintf("SpiceboxClass/%s#spec.resources.%s", className, d.Field)}},
		Unchanged: unchanged,
	}
	return q, notice
}

// capacityDefault picks the default answer for one class's dimension, a notice
// explaining it, and whether that default merely reaffirms the value already
// installed (see oap.Question.Unchanged) rather than proposing a new one.
//
// The notice is always non-empty, and whichever branch produces it must name
// the string actually being applied (def), never a human-readable rendering:
// the change reported has to match what a `kubectl get` of the applied CR
// shows.
func capacityDefault(className string, d capacityDimension, c schedfit.Ceiling, room schedfit.Headroom,
	declared resource.Quantity, floor, ceiling int64, live *unstructured.Unstructured) (def, notice string, unchanged bool) {

	if def, ok := installedDefault(live, d, floor, ceiling); ok {
		notice := fmt.Sprintf("SpiceboxClass %s: keeping the installed %s (%s); the bundle asks %s, which exceeds %s",
			className, d.Name, def, declared.String(), c.Source)
		// unchanged=true: def IS the live value, read back verbatim — applying
		// it is a byte-identical SSA no-op, not a new decision. A caller (see
		// cmd/oap's requireCapacityConsent) uses this to tell "nothing to
		// consent to" apart from "a fresh clamp", without parsing this notice's
		// prose (which is user-facing and free to reword).
		return def, notice, true
	}

	suggested := suggestFit(d, c, room, floor)
	suggestedQty := d.Qty(suggested)
	def = suggestedQty.String()

	notice = fmt.Sprintf("SpiceboxClass %s: lowering %s from %s to %s to fit %s",
		className, d.Name, declared.String(), def, c.Source)
	switch {
	case !room.Known:
		notice += fmt.Sprintf(" (could not read what is already running on %s, so this is only what the node could offer if it were idle — it may still not schedule under current load)", c.Node)
	case suggested > d.Headroom(room):
		// suggestFit floored the suggestion: the class cannot be lowered past
		// what its memory-backed scratch needs, and that floor is still bigger
		// than the free room. Saying only "lowering ... to fit <node>" here
		// would report a success that is not one — the pod will sit Pending.
		notice += fmt.Sprintf(" — but only %s of %s is free on %s right now, so it may still not schedule until something frees up",
			d.Human(d.Headroom(room)), d.Name, c.Node)
	}
	return def, notice, false
}

// installedDefault reports the default to offer when the class already
// installed on the cluster carries an in-[floor,ceiling] value for d, and
// whether one was found at all.
//
// THE quantity rule: prefer the RAW TEXT still on the live object
// (unstructured.NestedString) over any parsed-and-rendered form. Any trip
// through resource.Quantity discards the format the field was written in —
// "1.8Gi" comes back out of Quantity.String() as "1932735283200m" — and reusing
// that as the answer would rewrite the CR's text on every subsequent install
// though the value never changed (a byte-identical re-apply MUST be a no-op).
// Parsing IS used for the [floor,ceiling] comparison below, which needs only
// the value. A field authored as a bare JSON number has no format to preserve
// (NestedString errors on it), so that case falls back to Quantity.String().
func installedDefault(live *unstructured.Unstructured, d capacityDimension, floor, ceiling int64) (string, bool) {
	if live == nil {
		return "", false
	}
	res, err := classResources(live)
	if err != nil {
		return "", false
	}
	cur, ok := classRequests(res)[d.Res]
	if !ok {
		return "", false
	}
	if v := d.Unit(cur); v < floor || v > ceiling {
		return "", false
	}
	if s, found, err := unstructured.NestedString(live.Object, "spec", "resources", d.Field); err == nil && found {
		return s, true
	}
	return cur.String(), true
}
