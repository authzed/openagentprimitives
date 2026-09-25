package artifactcmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
)

func newArtifactGetCmd(g *apcmd.Globals) *cobra.Command {
	output := ""
	cmd := &cobra.Command{
		Use:   "get <ref>",
		Short: "Fetch an artifact by ArtifactStore ref",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArtifactGet(cmd.Context(), cmd.OutOrStdout(), g, args[0], output)
		},
	}
	apcmd.OutputFileFlag(cmd, &output, "")
	return cmd
}

func runArtifactGet(ctx context.Context, out io.Writer, g *apcmd.Globals, ref, output string) error {
	b, err := g.Bundle()
	if err != nil {
		return err
	}
	// Read the global debug bearer token.
	var sec corev1.Secret
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: "agentprimitives-system", Name: "spicebox-operator-debug-token"}, &sec); err != nil {
		return fmt.Errorf("read debug token Secret: %w", err)
	}
	token := string(sec.Data["token"])
	if token == "" {
		return fmt.Errorf("debug token Secret is empty")
	}

	// Port-forward.
	pf, err := portforward.New(b.REST, "agentprimitives-system", "spicebox-operator", "app.kubernetes.io/name=spicebox-operator", 8082, 0)
	if err != nil {
		return err
	}
	if err := pf.Start(ctx, io.Discard); err != nil {
		return err
	}
	defer pf.Stop()

	url := fmt.Sprintf("%s/debug/artifact?ref=%s", pf.URL(), urlQueryEscape(ref))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact get %s: status %d", ref, resp.StatusCode)
	}

	dst := out
	if output != "" {
		f, err := os.Create(output)
		if err != nil {
			return err
		}
		defer f.Close()
		dst = f
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
