package artifactcmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// styleTags renders an artifact's tags as bracketed tokens, drawn from the
// theme's own vocabulary: the "latest" tag is the current revision, so it takes
// the success color, and every other tag takes the accent that marks a title.
// The brackets carry the distinction on their own when color is off.
//
// The theme's Badge would otherwise be the natural style for a token like this,
// but Badge pads — and on colorless capabilities every style is the zero style,
// so a badge cell would be *narrower* off-color than on. A cell whose width
// depends on the color profile misaligns the column beside it, so tags stay
// foreground-only.
func styleTags(th *tui.Theme, tags []string) string {
	out := ""
	for i, tg := range tags {
		if i > 0 {
			out += " "
		}
		style := th.Title
		if tg == artifacts.TagLatest {
			style = th.Success
		}
		out += th.Render(style, "["+tg+"]")
	}
	return out
}

func renderRevisionTree(w io.Writer, th *tui.Theme, rows []artifacts.RevisionView) {
	for _, r := range rows {
		glyph := "•"
		if r.ParentID != "" {
			glyph = "└─"
		}
		fmt.Fprintf(w, "%s %s  %s  %q  %s  %s\n",
			th.Render(th.Subtle, glyph),
			th.Render(th.Title, fmt.Sprintf("#%d", r.Seq)),
			r.RevisionID, r.ChangeDescription,
			styleTags(th, r.Tags),
			th.Render(th.Subtle, fmt.Sprintf("%s %s", r.MIME, apcmd.HumanBytes(r.Size))),
		)
	}
}

func newArtifactVersionsListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list <session>",
		Short: "List logical artifacts in a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArtifactVersionsList(cmd.Context(), cmd.OutOrStdout(), g, args[0])
		},
	}
}

func runArtifactVersionsList(ctx context.Context, out io.Writer, g *apcmd.Globals, session string) error {
	conn, scope, err := connectArtifacts(ctx, g, session)
	if err != nil {
		return err
	}
	defer conn.Close()
	svc := artifacts.NewService(conn.Client, nil)
	arts, err := svc.ListArtifacts(ctx, scope)
	if err != nil {
		return err
	}
	if len(arts) == 0 {
		fmt.Fprintln(out, "no artifacts in this session")
		return nil
	}
	// Capabilities come from the stream this command writes to, so a pipe,
	// --no-color and NO_COLOR each land on the colorless theme.
	th := g.Theme(out)
	t := tui.NewTable(th, "NAME", "ARTIFACT-ID", "KIND", "#REVS", "TAGS", "AGE")
	for _, a := range arts {
		tags := make([]string, 0, len(a.Tags))
		for name := range a.Tags {
			tags = append(tags, name)
		}
		// Tag order comes from a map, so sort it: an unstable column would
		// change between two runs over identical data.
		sort.Strings(tags)
		t.Row(a.Name, a.ArtifactID, a.RendererKind, fmt.Sprintf("%d", a.RevisionCount), styleTags(th, tags), a.CreatedAt)
	}
	fmt.Fprint(out, t.Render())
	return nil
}

func newArtifactRevisionsCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "revisions <session> <artifact>",
		Short: "Show an artifact's revision tree",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, scope, err := connectArtifacts(cmd.Context(), g, args[0])
			if err != nil {
				return err
			}
			defer conn.Close()
			tree, err := artifacts.NewService(conn.Client, nil).RevisionTree(cmd.Context(), scope, args[1])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			th := g.Theme(out)
			fmt.Fprintln(out, th.Render(th.Title, args[1]))
			renderRevisionTree(out, th, tree)
			return nil
		},
	}
}

func newArtifactGetVersionedCmd(g *apcmd.Globals) *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "get <session> <ref>",
		Short: "Download a revision's bytes (ref = artifact-ID, artifact-ID#tag, or artrev-ID)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArtifactGetVersioned(cmd.Context(), cmd.OutOrStdout(), g, args[0], args[1], outPath)
		},
	}
	apcmd.OutputFileFlag(cmd, &outPath, "")
	return cmd
}

func connectArtifacts(ctx context.Context, g *apcmd.Globals, session string) (*memclient.Conn, memory.Scope, error) {
	b, err := g.Bundle()
	if err != nil {
		return nil, memory.Scope{}, err
	}
	conn, err := memclient.Connect(ctx, b, session, "")
	if err != nil {
		return nil, memory.Scope{}, err
	}
	return conn, conn.Scope, nil
}

func runArtifactGetVersioned(ctx context.Context, out io.Writer, g *apcmd.Globals, session, ref, outPath string) error {
	conn, scope, err := connectArtifacts(ctx, g, session)
	if err != nil {
		return err
	}
	defer conn.Close()
	renderName, err := artifacts.NewService(conn.Client, nil).ResolveToRender(ctx, scope, ref)
	if err != nil {
		return err
	}
	ns, name := splitScopeID(scope.ID)
	reqURL := fmt.Sprintf("%s/artifact/%s/%s/%s/output", conn.BaseURL(), ns, name, renderName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Carry the server's own message: the status alone cannot tell a
		// refused credential from a ref that resolved to a render this
		// session does not own, and both reach the user as a bare number.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if body := strings.TrimSpace(string(msg)); body != "" {
			return fmt.Errorf("artifact get: status %d: %s", resp.StatusCode, body)
		}
		return fmt.Errorf("artifact get: status %d", resp.StatusCode)
	}
	dst := out
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		dst = f
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

func splitScopeID(id string) (ns, name string) {
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			return id[:i], id[i+1:]
		}
	}
	return id, ""
}
