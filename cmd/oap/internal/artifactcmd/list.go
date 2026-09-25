package artifactcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
)

func newArtifactListCmd(g *apcmd.Globals) *cobra.Command {
	var prefix string
	var pageSize int
	var limit int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List artifacts in the ArtifactStore",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArtifactList(cmd.Context(), cmd.OutOrStdout(), g, prefix, pageSize, limit)
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "Only show artifacts whose key starts with prefix")
	cmd.Flags().IntVar(&pageSize, "page-size", 100, "Number of items per page (1-500)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum total items to return (0 = unlimited)")
	return cmd
}

// artifactListItem mirrors the JSON payload from /debug/artifacts.
type artifactListItem struct {
	Ref       string `json:"ref"`
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
}

type artifactListPayload struct {
	Items    []artifactListItem `json:"items"`
	Continue string             `json:"continue"`
}

func runArtifactList(ctx context.Context, out io.Writer, g *apcmd.Globals, prefix string, pageSize, limit int) error {
	// Clamp pageSize.
	if pageSize <= 0 {
		pageSize = 100
	}
	if pageSize > 500 {
		pageSize = 500
	}

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

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Capabilities come from the stream this command writes to, so a pipe,
	// --no-color and NO_COLOR each land on the colorless theme.
	t := g.Table(out, "KEY", "SIZE", "AGE")

	total := 0
	contToken := ""
	// truncated records that --limit, and not the store, ended the listing.
	truncated := false

	for {
		// Build query.
		q := url.Values{}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		q.Set("limit", fmt.Sprintf("%d", pageSize))
		if contToken != "" {
			q.Set("continue", contToken)
		}

		reqURL := fmt.Sprintf("%s/debug/artifacts?%s", pf.URL(), q.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return fmt.Errorf("artifact list: status %d", resp.StatusCode)
		}

		var payload artifactListPayload
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("decode response: %w", err)
		}
		_ = resp.Body.Close()

		take, leftBehind := pageWindow(limit, total, len(payload.Items))
		for _, item := range payload.Items[:take] {
			age := "<unknown>"
			if ts, err := time.Parse("2006-01-02T15:04:05Z", item.CreatedAt); err == nil {
				age = apcmd.DurationSinceShort(ts)
			}
			t.Row(item.Key, apcmd.HumanBytes(item.Size), age)
			total++
		}

		if leftBehind {
			truncated = true
			break
		}
		if limit > 0 && total >= limit {
			// The page ended exactly on the limit, so nothing was left behind
			// here; only the store's own continue token says whether the
			// listing had further pages to give.
			truncated = payload.Continue != ""
			break
		}
		if payload.Continue == "" {
			break
		}
		contToken = payload.Continue
	}

	if total == 0 {
		fmt.Fprintln(out, "no artifacts found")
		return nil
	}
	if truncated {
		if err := apcmd.TruncationNotice(out, "artifacts", limit); err != nil {
			return err
		}
	}
	fmt.Fprint(out, t.Render())
	return nil
}

// pageWindow decides how much of one page --limit still allows, and whether it
// leaves any of that page unshown.
//
// The two answers are separate because they mean different things: take drives
// the rendering, while leftBehind is the only place a listing can KNOW it was
// cut. A page consumed to its end, even one ending exactly on the limit, leaves
// nothing behind — whether more follows is then the store's continue token to
// answer, not this function's. limit 0 means unlimited.
func pageWindow(limit, shown, pageLen int) (take int, leftBehind bool) {
	if limit <= 0 {
		return pageLen, false
	}
	remaining := limit - shown
	if remaining < 0 {
		remaining = 0
	}
	if remaining >= pageLen {
		return pageLen, false
	}
	return remaining, true
}
