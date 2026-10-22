package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/authzed/openagentprimitives/examples/codecovbot/coverage-tool/internal/coverage"
	"github.com/spf13/cobra"
)

// Describe exposes exact block identities without revealing hit counts, so an
// AgentClass can classify source independently of whether tests exercised it.
func newDescribeCmd() *cobra.Command {
	var profile, source, file string
	var offset, limit int
	cmd := &cobra.Command{Use: "describe-blocks", Short: "List source-bound coverage blocks without hit counts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if profile == "" || source == "" {
				return fmt.Errorf("--profile and --source are required")
			}
			if offset < 0 || limit < 1 || limit > 1000 {
				return fmt.Errorf("--offset must be nonnegative and --limit must be between 1 and 1000")
			}
			data, err := os.ReadFile(profile)
			if err != nil {
				return err
			}
			report, err := coverage.Analyze(bytes.NewReader(data), source, coverage.DefaultPolicy(), coverage.Manifest{})
			if err != nil {
				return err
			}
			blocks := make([]coverage.Exclusion, 0)
			for _, b := range report.Blocks {
				if file == "" || b.File == file {
					blocks = append(blocks, b)
				}
			}
			total := len(blocks)
			start := min(offset, total)
			end := start + min(limit, total-start)
			var next *int
			if end < total {
				n := end
				next = &n
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				SchemaVersion string               `json:"schema_version"`
				Blocks        []coverage.Exclusion `json:"blocks"`
				Total         int                  `json:"total"`
				NextOffset    *int                 `json:"next_offset,omitempty"`
			}{"v1", blocks[start:end], total, next})
		}}
	f := cmd.Flags()
	f.StringVar(&profile, "profile", "", "Go coverage profile")
	f.StringVar(&source, "source", "", "Exact instrumented source module directory")
	f.StringVar(&file, "file", "", "Only this module-relative source file (default all)")
	f.IntVar(&offset, "offset", 0, "Start at this block offset")
	f.IntVar(&limit, "limit", 100, "Maximum blocks per page (1-1000)")
	return cmd
}
