package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// newAgentVerifyCmd verifies a pushed .oap agent container's cosign-format
// signature against a public key, without pulling or writing any bytes
// locally. It fails closed: any error oci.Verify returns (including the
// wrapped oci.ErrNoValidSignature for an unsigned, wrongly-signed, or
// tampered ref) is returned as-is, so cobra's RunE contract surfaces it as a
// non-zero exit — there is no partial or best-effort success path.
func newAgentVerifyCmd(_ *apcmd.Globals) *cobra.Command {
	var (
		keyPath   string
		plainHTTP bool
	)

	cmd := &cobra.Command{
		Use:   "verify <ref>",
		Short: "Verify a pushed .oap agent container's signature in its OCI registry",
		Long: "Verify a pushed .oap agent container's signature in its OCI registry.\n\n" +
			"<ref> is an OCI reference (\"host[:port]/name[:tag]\"). --key is the\n" +
			"PEM-encoded ECDSA public key (PKIX/SubjectPublicKeyInfo) matching the\n" +
			"private key `oap agent sign` used. Exits non-zero if no valid signature\n" +
			"is found.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]

			pub, err := loadPublicKeyPEM(keyPath)
			if err != nil {
				return fmt.Errorf("load verification key: %w", err)
			}

			verifiedDigest, err := oci.Verify(cmd.Context(), ref, pub, oci.Options{PlainHTTP: plainHTTP})
			if err != nil {
				return err
			}

			// Print the exact verified digest, not just the (possibly mutable)
			// ref the user typed — that's the identity the signature actually
			// covers and what a caller should pin to.
			fmt.Fprintf(cmd.OutOrStdout(), "OK: %s verified (digest %s)\n", ref, verifiedDigest)
			return nil
		},
	}

	cmd.Flags().StringVar(&keyPath, "key", "", "Path to a PEM-encoded ECDSA public key (PKIX) to verify against (required)")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP (no TLS) to reach the registry — for loopback/dev registries")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}
