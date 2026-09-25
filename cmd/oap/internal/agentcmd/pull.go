package agentcmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// newAgentPullCmd pulls a .oap agent container from an OCI registry and
// writes it to a local file (or stdout, via `-o -`).
//
// --verify fails closed: when set, --key (a PEM ECDSA public key) is
// required, and oci.Verify is run against ref's current manifest and MUST
// succeed before oci.Pull is even called — an unsigned, wrongly-signed, or
// tampered ref never reaches the local output path or stdout. The subsequent
// pull is pinned to the exact digest Verify returned, so a mutable tag can't
// be re-resolved to different (unsigned) content between the verify and the
// pull.
func newAgentPullCmd(_ *apcmd.Globals) *cobra.Command {
	var (
		out       string
		plainHTTP bool
		verify    bool
		keyPath   string
	)

	cmd := &cobra.Command{
		Use:   "pull <ref>",
		Short: "Pull a .oap agent container from an OCI registry",
		Long: "Pull a .oap agent container from an OCI registry.\n\n" +
			"<ref> is an OCI reference (\"host[:port]/name[:tag]\").\n\n" +
			"--verify checks ref's cosign-format signature against --key before\n" +
			"writing anything locally (see `oap agent verify`); a failed check writes\n" +
			"nothing and exits non-zero.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			opts := oci.Options{PlainHTTP: plainHTTP}

			// pullRef is what we actually fetch. When --verify is set it becomes
			// a digest-pinned form of ref so the bytes we pull are provably the
			// exact manifest we verified — a re-resolved mutable tag can't sneak
			// different content in between the verify and the pull (TOCTOU).
			pullRef := ref
			if verify {
				if keyPath == "" {
					return fmt.Errorf("--verify requires --key <public-key.pem>")
				}
				pub, err := loadPublicKeyPEM(keyPath)
				if err != nil {
					return fmt.Errorf("load verification key: %w", err)
				}
				// Verify against the registry BEFORE pulling or writing anything
				// locally — fail closed: an unsigned/tampered ref must never reach
				// outPath/stdout, and must not even be pulled.
				verifiedDigest, err := oci.Verify(cmd.Context(), ref, pub, opts)
				if err != nil {
					return err
				}
				// Pin the pull to the digest we just verified, so Pull can't
				// independently re-resolve ref's tag to a different (unsigned)
				// manifest. For an already-digest-pinned ref this reproduces the
				// same digest.
				pullRef, err = oci.PinnedRef(ref, verifiedDigest)
				if err != nil {
					return fmt.Errorf("pin verified digest for %s: %w", ref, err)
				}
			}

			packed, dig, err := oci.Pull(cmd.Context(), pullRef, opts)
			if err != nil {
				return fmt.Errorf("pull %s: %w", pullRef, err)
			}

			outPath := out
			if outPath == "" {
				outPath, err = oci.DefaultLocalName(ref)
				if err != nil {
					return fmt.Errorf("determine default output filename for %s: %w", ref, err)
				}
			}

			if outPath == "-" {
				if _, err := cmd.OutOrStdout().Write(packed); err != nil {
					return fmt.Errorf("write pulled bundle to stdout: %w", err)
				}
			} else if err := writeFileAtomic(outPath, packed, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", outPath, err)
			}

			// Digest + output path go to stderr (not stdout), matching `agent
			// package`'s convention, so `-o -` piping stays clean.
			fmt.Fprintf(cmd.ErrOrStderr(), "%s  %s\n", dig, outPath)
			return nil
		},
	}

	cmd.Flags().StringVarP(&out, "output", "o", "", "Output path for the pulled .oap ('-' for stdout; default: <repo-basename>.oap in the current directory)")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP (no TLS) to reach the registry — for loopback/dev registries")
	cmd.Flags().BoolVar(&verify, "verify", false, "Verify ref's cosign-format signature against --key before writing anything locally (fails closed)")
	cmd.Flags().StringVar(&keyPath, "key", "", "Path to a PEM-encoded ECDSA public key (PKIX) to verify against (required with --verify)")
	return cmd
}

// writeFileAtomic writes data to a temp file in the same directory as path,
// then renames it into place. A mid-write failure (disk full, crash, an
// aborted pull) leaves only a discarded temp file, never a truncated/partial
// .oap at path — os.WriteFile, by contrast, truncates path up front and would
// leave a corrupt file behind. The temp file shares path's directory so the
// rename is a same-filesystem atomic operation.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: harmless no-op once the rename below succeeds (the
	// temp name no longer exists), and removes the partial temp on any error
	// path before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
