package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// newAgentSignCmd signs an already-pushed .oap agent container in place in
// its OCI registry (see `oap agent push`): it attaches a cosign-format
// signature referrer to ref's current manifest via oci.Sign, without
// touching any local .oap bytes. The signing key is loaded from a local PEM
// file — no KMS/Fulcio/keyless integration yet.
func newAgentSignCmd(_ *apcmd.Globals) *cobra.Command {
	var (
		keyPath   string
		plainHTTP bool
	)

	cmd := &cobra.Command{
		Use:   "sign <ref>",
		Short: "Sign a pushed .oap agent container in its OCI registry",
		Long: "Sign a pushed .oap agent container in its OCI registry.\n\n" +
			"<ref> is an OCI reference (\"host[:port]/name[:tag]\") that must already\n" +
			"have been pushed (see `oap agent push`). --key is a PEM-encoded ECDSA\n" +
			"private key (PKCS#8 or SEC1).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]

			signer, err := loadPrivateKeySigner(keyPath)
			if err != nil {
				return fmt.Errorf("load signing key: %w", err)
			}

			sigDigest, err := oci.Sign(cmd.Context(), ref, signer, oci.Options{PlainHTTP: plainHTTP})
			if err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), sigDigest)
			return nil
		},
	}

	cmd.Flags().StringVar(&keyPath, "key", "", "Path to a PEM-encoded ECDSA private key (PKCS#8 or SEC1) to sign with (required)")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP (no TLS) to reach the registry — for loopback/dev registries")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}
