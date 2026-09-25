package install

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Conflict is one pre-existing cluster object a bundled CR — or a Secret this
// install synthesizes from an answered secret question — would seize if it
// were force-applied. The ownership guards produce these as DATA rather than
// erroring on the first one, so a single run can report every collision at
// once and give the caller a chance to adopt them deliberately.
type Conflict struct {
	Kind      string
	Namespace string // empty for a cluster-scoped object
	Name      string
	// Secret marks a Secret this install would seize. Adopting one OVERWRITES
	// its data (a bundled CR's spec, by contrast, converges), so a blanket adopt
	// never covers it — only naming it explicitly does. Derived from the object's
	// own Kind at the single construction site (checkResourceOwnership), never
	// from which guard produced the Conflict.
	Secret bool
	// ClusterScoped marks shared infra. Never adoptable through this path:
	// requires.clusterDeps is the declared way to adopt a shared cluster dep,
	// and it PRESERVES the resource's spec instead of seizing it.
	ClusterScoped bool
	// adoptionRefused marks a conflict whose ownership cannot be transferred
	// through any surface. Channel credential Secrets use it because seizing
	// their data and uninstall eligibility is not an adoption operation.
	adoptionRefused bool
}

// Key is the "Kind/Name" identity a caller names in InstallOpts.Adopt.
func (c Conflict) Key() string { return c.Kind + "/" + c.Name }

// AdoptionRefused reports that this collision cannot be transferred through
// an install surface, even when its exact key is named.
func (c Conflict) AdoptionRefused() bool { return c.adoptionRefused }

// String renders one conflict for an error or prompt line: namespaced objects
// read "Kind ns/name", cluster-scoped ones "Kind/name".
func (c Conflict) String() string {
	if c.Namespace == "" {
		return c.Kind + "/" + c.Name
	}
	return fmt.Sprintf("%s %s/%s", c.Kind, c.Namespace, c.Name)
}

// ConflictError is returned when at least one pre-existing object would be
// seized and was not adopted. It carries EVERY such object, not just the first,
// and aborts Install before any cluster write.
//
// Error() deliberately says only what holds on every install surface: the
// object list and the consequence of adopting one. It prescribes NO remediation
// mechanism — a "--adopt=Kind/Name" hint here reached a menu-bar user as a
// native notification and admind's HTTP body verbatim, the same class of bug
// the "no channel-specifics in a generic message" rule exists to prevent. Each
// surface renders its own affordance from e.Conflicts instead: the CLI wraps
// this error with its own --adopt guidance (agentcmd's wrapConflictError), the
// desktop dialog and the admin UI build theirs from the Conflicts a
// 409/AdoptDecision call already carries, never from this string.
type ConflictError struct{ Conflicts []Conflict }

func (e *ConflictError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "install: refusing to overwrite %d pre-existing object(s) not managed by this install:", len(e.Conflicts))
	for _, c := range e.Conflicts {
		fmt.Fprintf(&b, "\n  - %s", c)
	}
	b.WriteString("; an adopted object's spec is overwritten with this bundle's, and `oap agent uninstall` will delete it.")
	for _, c := range e.Conflicts {
		if c.adoptionRefused {
			b.WriteString(" Planned channel credential Secrets cannot be adopted; choose a different channel name or remove the conflicting Secret out of band.")
			break
		}
	}
	return b.String()
}

// resolveConflicts decides which of conflicts this install may adopt and
// returns the adopted keys, or an error that aborts Install before any write.
// The order is deliberate:
//
//  1. A cluster-scoped conflict is NEVER adoptable and dominates every other
//     finding — no flag can fix it, so reporting it alone is clearer than
//     burying it among things --adopt could resolve.
//  2. An --adopt key naming no actual conflict is a hard error, mirroring
//     Resolve's rejectUnknownKeys for --set: a typo must not read as "adopted"
//     while the real conflict still aborts the install. Zero conflicts (below)
//     skips this check entirely, deliberately: that is the already-converged
//     re-install case, where the operator re-runs the same --adopt=Kind/Name
//     against an object that now carries this install's label. Erroring there
//     would break idempotent scripted re-install.
//  3. Explicit keys, then AdoptAll (non-Secret only — adopting a Secret
//     overwrites its data, so it always takes an individual act).
//  4. The AdoptDecision hook runs ONLY when neither flag was set. Flags are
//     authoritative, so a scripted install stays deterministic even on a TTY.
//  5. Anything left is a *ConflictError.
func resolveConflicts(ctx context.Context, conflicts []Conflict, opts InstallOpts) ([]string, error) {
	// No collisions to decide: opts.Adopt/AdoptAll/AdoptDecision are not even
	// consulted, by design — see rule 2's deliberate-exception note above.
	if len(conflicts) == 0 {
		return nil, nil
	}
	ordered := make([]Conflict, len(conflicts))
	copy(ordered, conflicts)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() < ordered[j].String() })

	for _, c := range ordered {
		if c.ClusterScoped {
			return nil, fmt.Errorf("install: cluster-scoped %s already exists and is not managed by this install; refusing to overwrite shared infra (declare it under requires.clusterDeps to adopt it, or remove it from the bundle)", c.Key())
		}
	}

	byKey := make(map[string]Conflict, len(ordered))
	for _, c := range ordered {
		byKey[c.Key()] = c
	}

	var unknown []string
	for _, k := range opts.Adopt {
		if _, ok := byKey[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("install: --adopt names %v, which is not a conflicting object in this install; the conflicts are %v", unknown, conflictKeys(ordered))
	}
	var categoricallyRefused []string
	for _, c := range ordered {
		if c.adoptionRefused {
			categoricallyRefused = append(categoricallyRefused, c.Key())
		}
	}
	if len(categoricallyRefused) > 0 {
		for _, requested := range opts.Adopt {
			if slices.Contains(categoricallyRefused, requested) {
				sort.Strings(categoricallyRefused)
				return nil, fmt.Errorf("install: refusing to adopt %v: planned channel credential Secrets cannot be adopted because that would overwrite credentials and transfer uninstall ownership", categoricallyRefused)
			}
		}
		if opts.AdoptAll || opts.AdoptDecision == nil {
			return nil, &ConflictError{Conflicts: ordered}
		}
	}

	adopted := make(map[string]bool, len(ordered))
	for _, k := range opts.Adopt {
		adopted[k] = true
	}
	if opts.AdoptAll {
		for _, c := range ordered {
			if !c.Secret {
				adopted[c.Key()] = true
			}
		}
	}

	if !opts.AdoptAll && len(opts.Adopt) == 0 && opts.AdoptDecision != nil {
		keys, err := opts.AdoptDecision(ctx, ordered)
		if err != nil {
			return nil, fmt.Errorf("install: adopt decision: %w", err)
		}
		for _, k := range keys {
			if _, ok := byKey[k]; !ok {
				return nil, fmt.Errorf("install: adopt decision returned %q, which is not a conflicting object in this install", k)
			}
			adopted[k] = true
		}
	}
	for _, c := range ordered {
		if c.adoptionRefused && adopted[c.Key()] {
			return nil, fmt.Errorf("install: refusing to adopt %s: planned channel credential Secrets cannot be adopted because that would overwrite credentials and transfer uninstall ownership", c.Key())
		}
	}

	// Adopting a Secret OVERWRITES its data, which is why AdoptAll has always
	// carved Secrets out: seizing one has to be an individual act. Over HTTP it
	// was not one — `adopt` is a JSON array in the same POST body as everything
	// else, so naming a Secret there is the same single request as a blanket
	// adopt, and the distinction the carve-out rests on does not exist.
	//
	// So the permission is a property of the CALLER (AdoptSecretsAllowed,
	// default refused), checked once here over EVERY route into `adopted` —
	// explicit keys and the AdoptDecision hook alike — rather than at each
	// surface, where the next surface would have to remember.
	if !opts.AdoptSecretsAllowed {
		var refusedSecrets []string
		for _, c := range ordered {
			if c.Secret && adopted[c.Key()] {
				refusedSecrets = append(refusedSecrets, c.Key())
			}
		}
		if len(refusedSecrets) > 0 {
			sort.Strings(refusedSecrets)
			return nil, fmt.Errorf("install: refusing to adopt %v from this surface: adopting a Secret overwrites its data, which takes an individual act by an operator who can see the specific object; resolve it out of band or install from the CLI", refusedSecrets)
		}
	}

	var remaining []Conflict
	for _, c := range ordered {
		if !adopted[c.Key()] {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) > 0 {
		return nil, &ConflictError{Conflicts: remaining}
	}

	keys := make([]string, 0, len(adopted))
	for k := range adopted {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func conflictKeys(conflicts []Conflict) []string {
	keys := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		keys = append(keys, c.Key())
	}
	return keys
}
