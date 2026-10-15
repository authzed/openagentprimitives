package main

import (
	"fmt"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
)

// newBootstrapSecretsCmd is the pre-start bootstrap Job's entrypoint: it mints
// every internal Secret a GitOps-applied install needs (idempotently, never
// rotating) and exits, WITHOUT starting the manager. It runs before the operator
// (and before SpiceDB), so the operator and the data-plane pods mount their
// Secrets cleanly at first boot — no mint-after-start, no pod rolls.
//
// It uses a direct (uncached) client, which reads live and so is both a correct
// reader and writer for the create-if-absent mint logic. The default client-go
// scheme already knows corev1.Secret.
func newBootstrapSecretsCmd() *cobra.Command {
	var skipSpiceDBToken, skipPostgresToken bool
	cmd := &cobra.Command{
		Use:          "bootstrap-secrets",
		Short:        "Mint the internal Secrets a GitOps-applied install needs, then exit (run as a pre-start Job).",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := ctrl.GetConfig()
			if err != nil {
				return fmt.Errorf("load kube config: %w", err)
			}
			cl, err := client.New(cfg, client.Options{})
			if err != nil {
				return fmt.Errorf("build client: %w", err)
			}
			return bootstrap.MintAllInternalSecrets(cmd.Context(), cl, cl, bootstrap.MintOptions{
				SkipSpiceDBToken:  skipSpiceDBToken,
				SkipPostgresToken: skipPostgresToken,
			})
		},
	}
	cmd.Flags().BoolVar(&skipSpiceDBToken, "skip-spicedb-token", false,
		"Do not mint spicebox-spicedb-token (external SpiceDB: the pre-shared key is yours to provide)")
	cmd.Flags().BoolVar(&skipPostgresToken, "skip-postgres-token", false,
		"Do not mint spicebox-postgres-token (external Postgres: the URI is yours to provide)")
	return cmd
}
