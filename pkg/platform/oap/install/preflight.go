// Package install implements the fail-closed checks that gate a .oap install
// before any cluster write happens.
package install

import (
	"context"
	"fmt"

	semver "github.com/Masterminds/semver/v3"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// Preflight decides whether b is safe to install against a cluster reporting
// clusterApVersion. It runs, in order, returning on the first failure:
//
//  1. Structural validation via b.Validate(), which also rejects an unknown
//     oapFormatVersion major.
//  2. The compat floor: when the manifest declares Compat.MinApVersion and
//     clusterApVersion is non-empty, the cluster must be >= the floor.
//  3. Requires coherence: every Requires.Secrets[] entry that NAMES a
//     Questions[] entry must name one of Type == QSecret carrying a
//     createSecret target, so install can both prompt for the value and put it
//     where the bundled CRs read it. An entry naming no question is coherent on
//     its own — see checkRequiresCoherence for what satisfies each shape.
//
// Preflight is read-only: it never contacts a cluster and never mutates b. The
// caller resolves clusterApVersion and passes it in. A nil return means every
// check passed.
func Preflight(ctx context.Context, b *oap.Bundle, clusterApVersion string) error {
	_ = ctx // preflight is pure/local; ctx is accepted for call-site consistency and future cancellation.

	if b == nil || b.Manifest == nil {
		return fmt.Errorf("preflight: bundle has no manifest")
	}

	if err := b.Validate(); err != nil {
		return fmt.Errorf("bundle invalid: %w", err)
	}

	if err := checkCompat(b.Manifest, clusterApVersion); err != nil {
		return err
	}

	if err := checkRequiresCoherence(b.Manifest); err != nil {
		return err
	}

	return nil
}

// checkCompat enforces Compat.MinApVersion as a hard floor on clusterApVersion.
// Either side being unset skips the check (there is nothing to compare); either
// side failing to parse as semver is a hard error rather than a silent pass.
func checkCompat(m *oap.Manifest, clusterApVersion string) error {
	floor := m.Compat.MinApVersion
	if floor == "" || clusterApVersion == "" {
		return nil
	}

	floorVer, err := semver.NewVersion(floor)
	if err != nil {
		return fmt.Errorf("compat.minApVersion %q is not valid semver: %w", floor, err)
	}
	clusterVer, err := semver.NewVersion(clusterApVersion)
	if err != nil {
		return fmt.Errorf("cluster oap version %q is not valid semver: %w", clusterApVersion, err)
	}
	if clusterVer.LessThan(floorVer) {
		return fmt.Errorf("agent requires oap >= %s but cluster is %s", floor, clusterApVersion)
	}
	return nil
}

// checkRequiresCoherence confirms every declared required secret can actually
// be satisfied at install time. A required secret whose question collects a
// value with nowhere to put it would otherwise surface as an agent referencing
// a Secret nobody created, long after the install reported success.
//
// An entry that names NO question is coherent whichever shape it is, and the
// two shapes are satisfied differently:
//
//   - Keyed. RequiredSecretQuestions synthesizes one question per key at
//     install time, so declaring the keys is all an author has to do to get the
//     credential collected and the Secret created.
//   - Keyless. It names a Secret as a whole rather than a value a person can
//     type (an OAuth token set, a channel wizard's credential bundle), which no
//     single typed answer could reconstruct. Install collects nothing for it and
//     says so; whatever mints that shape mints it.
//
// Requiring a hand-written question for the keyed shape is what this check used
// to do, and it pushed authors toward declaring keyless entries they did not
// mean — a Secret with one known key, declared as a whole, purely to get past
// the rule. Then nothing asked for it.
func checkRequiresCoherence(m *oap.Manifest) error {
	secretQuestions := make(map[string]oap.Question, len(m.Questions))
	for _, q := range m.Questions {
		if q.Type == oap.QSecret {
			secretQuestions[q.Name] = q
		}
	}

	for _, rs := range m.Requires.Secrets {
		if rs.Question == "" {
			continue
		}
		q, ok := secretQuestions[rs.Question]
		if !ok {
			return fmt.Errorf("requires.secrets[%s]: question %q not found among manifest questions (or is not type=secret)", rs.Name, rs.Question)
		}
		// Mirrors Resolve, which refuses such an answer rather than dropping
		// it: catching it here fails the install BEFORE anyone is asked to
		// type a credential that has nowhere to go.
		if q.Secret == nil || q.Secret.CreateSecret == nil {
			return fmt.Errorf("requires.secrets[%s]: question %q declares no createSecret target, so install has nowhere to put the value it collects", rs.Name, rs.Question)
		}
	}
	return nil
}
