package clidocs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerate(t *testing.T) {
	root := &cobra.Command{Use: "oap", Short: "the CLI"}
	root.PersistentFlags().StringP("namespace", "n", "", "Kubernetes namespace")

	agent := &cobra.Command{Use: "agent", Short: "Manage agents"}
	run := &cobra.Command{
		Use:     "run <agent-class>",
		Short:   "Run a session",
		Example: "oap agent run pirate --prompt hi",
		RunE:    func(*cobra.Command, []string) error { return nil },
	}
	run.Flags().String("prompt", "", "Inline prompt with <braces> and {curly}")
	run.Flags().Bool("no-tail", false, "Apply and exit")
	hidden := &cobra.Command{Use: "secret", Hidden: true}
	agent.AddCommand(run, hidden)
	root.AddCommand(agent)

	dir := t.TempDir()
	require.NoError(t, Generate(root, dir))

	overB, err := os.ReadFile(filepath.Join(dir, "cli-reference.mdx"))
	require.NoError(t, err)
	over := string(overB)
	assert.Contains(t, over, "section: 'Reference'")
	assert.Contains(t, over, "group: 'CLI'")
	assert.Contains(t, over, "[`oap agent`](/docs/oap-agent)", "family listed in overview")
	assert.Contains(t, over, "--namespace", "global persistent flags rendered on the overview")

	famB, err := os.ReadFile(filepath.Join(dir, "oap-agent.mdx"))
	require.NoError(t, err)
	fam := string(famB)
	assert.Contains(t, fam, "title: 'oap agent'")
	assert.Contains(t, fam, "`oap agent run <agent-class>`", "usage heading is backtick-wrapped so <args> stay literal")
	assert.Contains(t, fam, "--prompt string", "local flag rendered")
	assert.Contains(t, fam, "oap agent run pirate", "example rendered")
	assert.NotContains(t, fam, "secret", "hidden subcommand skipped")
	assert.NotContains(t, fam, "--namespace", "inherited persistent flags not repeated per command")

	_, statErr := os.Stat(filepath.Join(dir, "oap-secret.mdx"))
	assert.True(t, os.IsNotExist(statErr), "no page for a hidden family")
}
