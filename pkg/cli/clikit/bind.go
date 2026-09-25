// Package clikit holds the shared cobra wiring for the agentprimitives server
// binaries (operator, channelsd, authzd, webd, runner): typed flags whose
// values may come from the environment, routed through pflag so a malformed
// env value fails CLOSED at startup instead of silently falling back to a
// default.
//
// Why not viper / cobrautil's auto-prefix: our env vars are deliberately
// shared and unprefixed across binaries (NATS_URL is read by five of them),
// set once per namespace and consumed by many pods. A per-binary prefix
// (CHANNELSD_NATS_URL) would break every Deployment/ConfigMap. So instead of
// an automatic flag→PREFIX_FLAG mapping, each binary declares an explicit
// flag→env-var binding; the names shared across binaries live in globalenvs.go.
package clikit

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"
)

// EnvOverridePreRunE returns a cobra PreRunE that populates flags from the
// environment. For each (flagName → envVar) binding it sets the flag from the
// env var when BOTH:
//
//   - the flag was NOT given on the command line (an explicit flag always
//     wins over the environment), and
//   - the env var is set to a non-empty value (empty is treated as unset, so a
//     flag's declared default stands — matching the prior env.Or semantics).
//
// The value is applied via pflag's Set, so it goes through the SAME typed
// parser as a command-line flag: a malformed duration/int/bool is a startup
// error naming the env var, not a silent default. All binding errors are
// collected (sorted for determinism) and returned together via errors.Join so
// an operator sees every bad value at once rather than one-at-a-time.
//
// A binding that names a flag which is not registered is a programming error
// and is reported the same way (so a typo surfaces loudly in tests/CI rather
// than silently dropping the binding).
func EnvOverridePreRunE(bindings map[string]string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		names := make([]string, 0, len(bindings))
		for n := range bindings {
			names = append(names, n)
		}
		sort.Strings(names)

		var errs []error
		for _, flagName := range names {
			envVar := bindings[flagName]
			f := cmd.Flags().Lookup(flagName)
			if f == nil {
				errs = append(errs, fmt.Errorf("clikit: flag %q (bound to %s) is not registered", flagName, envVar))
				continue
			}
			if f.Changed {
				continue // explicit command-line value wins over the environment
			}
			if v := os.Getenv(envVar); v != "" {
				if err := cmd.Flags().Set(flagName, v); err != nil {
					errs = append(errs, fmt.Errorf("%s=%q: %w", envVar, v, err))
				}
			}
		}
		return errors.Join(errs...)
	}
}

// ChainPreRunE composes several cobra PreRunE functions into one, running them
// in order and stopping at the first error. It lets a binary add its own
// PreRunE alongside EnvOverridePreRunE without either clobbering the other
// (cobra allows only a single PreRunE per command).
func ChainPreRunE(fns ...func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		for _, fn := range fns {
			if fn == nil {
				continue
			}
			if err := fn(cmd, args); err != nil {
				return err
			}
		}
		return nil
	}
}
