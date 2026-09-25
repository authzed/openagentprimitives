package agentcmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// loadBundle reads a .oap source: either a packed file or a folder-source
// directory. Folder-vs-file is decided by os.Stat, never by filename suffix —
// the format is identified by content, and any extension (or none) must work
// for a packed file. It returns the decoded bundle and the packed bytes (for
// digesting).
func loadBundle(path string) (*oap.Bundle, []byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		b, err := oap.FromFolder(path)
		if err != nil {
			return nil, nil, err
		}
		packed, err := oap.Pack(b)
		if err != nil {
			return nil, nil, err
		}
		return b, packed, nil
	}
	packed, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	b, err := oap.Unpack(packed)
	if err != nil {
		return nil, nil, err
	}
	return b, packed, nil
}

func newAgentInspectCmd(_ *apcmd.Globals) *cobra.Command {
	var readmeOnly bool
	cmd := &cobra.Command{
		Use:   "inspect <path-to-oap-or-folder>",
		Short: "Print the manifest, questions, requirements, and digest of an agent container (.oap)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, packed, err := loadBundle(args[0])
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()

			if readmeOnly {
				if len(b.Readme) == 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "(no README.md in this bundle)")
					return nil
				}
				_, err := w.Write(b.Readme)
				return err
			}

			dig, err := oap.Digest(packed)
			if err != nil {
				return err
			}
			// Folder manifests retain authoring paths, while the packed config
			// carries the content-addressed dependency descriptors operators need
			// to inspect. Decode the bytes we just digested so both source forms
			// report the exact same dependency names, versions, and digests.
			b, err = oap.Unpack(packed)
			if err != nil {
				return fmt.Errorf("inspect packed bundle: %w", err)
			}
			m := b.Manifest
			fmt.Fprintf(w, "Agent:    %s  v%s\n", m.Agent.Name, m.Agent.Version)
			if m.Agent.DisplayName != "" {
				fmt.Fprintf(w, "Title:    %s\n", m.Agent.DisplayName)
			}
			fmt.Fprintf(w, "Format:   v%s\n", m.OapFormatVersion)
			fmt.Fprintf(w, "Digest:   %s\n", dig)
			if len(b.Dependencies) > 0 {
				if _, err := fmt.Fprintln(w, "Embedded agents:"); err != nil {
					return err
				}
				if err := writeEmbeddedAgents(w, b, nil); err != nil {
					return err
				}
			}
			if m.Compat.MinApVersion != "" {
				fmt.Fprintf(w, "Requires: oap >= %s\n", m.Compat.MinApVersion)
			}
			if len(b.Readme) > 0 {
				fmt.Fprintf(w, "Docs:     README.md (%s)\n", apcmd.HumanBytes(int64(len(b.Readme))))
			}
			if len(m.Requires.Secrets) > 0 {
				fmt.Fprintln(w, "Secrets needed:")
				for _, s := range m.Requires.Secrets {
					fmt.Fprintf(w, "  - %s (keys: %v) %s\n", s.Name, s.Keys, s.Purpose)
				}
			}
			if len(m.Questions) > 0 {
				fmt.Fprintln(w, "Install questions:")
				for _, q := range m.Questions {
					fmt.Fprintf(w, "  - %s [%s] %s\n", q.Name, q.Type, q.Prompt)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&readmeOnly, "readme", false, "Print the bundle's README.md (raw markdown) instead of the summary")
	return cmd
}

func writeEmbeddedAgents(w io.Writer, bundle *oap.Bundle, parent oap.DependencyPath) error {
	for _, dependency := range bundle.Dependencies {
		name := dependency.Descriptor.Name
		path := append(append(oap.DependencyPath(nil), parent...), name)
		if _, err := fmt.Fprintf(w, "  - %s  v%s  %s\n", path.String(), dependency.Descriptor.Version, dependency.Descriptor.Digest); err != nil {
			return err
		}
		if err := writeEmbeddedAgents(w, dependency.Bundle, path); err != nil {
			return err
		}
	}
	return nil
}
