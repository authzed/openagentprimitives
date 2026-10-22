// Package cli provides local static coverage analysis without cluster access.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/authzed/openagentprimitives/examples/codecovbot/coverage-tool/internal/coverage"
	"github.com/authzed/openagentprimitives/examples/codecovbot/coverage-tool/internal/coverage/artifact"
	"github.com/spf13/cobra"
)

func NewCmd() *cobra.Command {
	root := &cobra.Command{Use: "codecovbot-tool", Short: "Analyze Go executable statement coverage offline"}
	var profile, source, policy, exclusions, output string
	var policyText, policyJSON, exclusionsJSON, baseReport, preferred string
	var threshold float64
	analyze := &cobra.Command{Use: "analyze", Short: "Compute raw and policy-filtered functional statement coverage", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if profile == "" || source == "" {
			return fmt.Errorf("--profile and --source are required")
		}
		policyInputs := 0
		if policy != "" {
			policyInputs++
		}
		if cmd.Flags().Changed("policy-text") {
			policyInputs++
		}
		if cmd.Flags().Changed("policy-json") {
			policyInputs++
		}
		if policyInputs > 1 {
			return fmt.Errorf("--policy, --policy-text and --policy-json are mutually exclusive")
		}
		if exclusions != "" && cmd.Flags().Changed("exclusions-json") {
			return fmt.Errorf("--exclusions and --exclusions-json are mutually exclusive")
		}
		p := coverage.DefaultPolicy()
		if cmd.Flags().Changed("policy-text") {
			p.Text = policyText
		}
		if policy != "" {
			b, err := os.ReadFile(policy)
			if err != nil {
				return err
			}
			p, err = coverage.ReadPolicy(bytes.NewReader(b))
			if err != nil {
				return fmt.Errorf("policy: %w", err)
			}
		}
		if cmd.Flags().Changed("policy-json") {
			var err error
			p, err = coverage.ReadPolicy(bytes.NewBufferString(policyJSON))
			if err != nil {
				return fmt.Errorf("policy: %w", err)
			}
		}
		var m coverage.Manifest
		if exclusions != "" {
			b, err := os.ReadFile(exclusions)
			if err != nil {
				return err
			}
			m, err = coverage.ReadManifest(bytes.NewReader(b))
			if err != nil {
				return fmt.Errorf("exclusions: %w", err)
			}
		}
		if cmd.Flags().Changed("exclusions-json") {
			var err error
			m, err = coverage.ReadManifest(bytes.NewBufferString(exclusionsJSON))
			if err != nil {
				return fmt.Errorf("exclusions: %w", err)
			}
		}
		b, err := os.ReadFile(profile)
		if err != nil {
			return err
		}
		report, err := coverage.Analyze(bytes.NewReader(b), source, p, m)
		if err != nil {
			return err
		}
		var base *coverage.Report
		if baseReport != "" {
			b, err := os.ReadFile(baseReport)
			if err != nil {
				return err
			}
			r, err := coverage.ReadReport(bytes.NewReader(b))
			if err != nil {
				return err
			}
			base = &r
		}
		comparison, err := coverage.Compare(report, base, preferred, threshold)
		if err != nil {
			return err
		}
		report.Comparison = &comparison
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
		if output != "" {
			return os.WriteFile(output, encoded, 0600)
		}
		_, err = cmd.OutOrStdout().Write(encoded)
		return err
	}}
	f := analyze.Flags()
	f.StringVar(&profile, "profile", "", "Go coverage profile (concatenated profiles supported)")
	f.StringVar(&source, "source", "", "Exact instrumented source module directory")
	f.StringVar(&policy, "policy", "", "Versioned operator policy JSON")
	f.StringVar(&exclusions, "exclusions", "", "Reviewed exact-source exclusions JSON")
	f.StringVarP(&output, "output", "o", "", "Write report JSON to this file (default stdout)")
	f.StringVar(&policyText, "policy-text", "", "Opaque operator policy text (mutually exclusive with --policy)")
	f.StringVar(&policyJSON, "policy-json", "", "Inline structured policy JSON, including explicit exclusion rules")
	f.StringVar(&exclusionsJSON, "exclusions-json", "", "Reviewed exclusion manifest JSON (mutually exclusive with --exclusions)")
	f.StringVar(&baseReport, "base-report", "", "Authenticated exact-base analysis report JSON")
	f.StringVar(&preferred, "preferred-metric", "functional", "Comparison metric: functional or raw")
	f.Float64Var(&threshold, "threshold", 0, "Maximum allowed drop in percentage points")
	root.AddCommand(analyze)
	root.AddCommand(newFetchCmd())
	root.AddCommand(newDescribeCmd())
	return root
}

func newFetchCmd() *cobra.Command {
	var opts artifact.Options
	cmd := &cobra.Command{Use: "fetch-artifact", Short: "Fetch and safely extract a verified GitHub coverage artifact", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		opts.Token = os.Getenv("GITHUB_TOKEN")
		result, err := (artifact.Fetcher{}).Fetch(cmd.Context(), opts)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	f := cmd.Flags()
	f.StringVar(&opts.Repository, "repository", "", "GitHub owner/repository")
	f.Int64Var(&opts.RunID, "run-id", 0, "Authenticated workflow run ID")
	f.Int64Var(&opts.Attempt, "attempt", 0, "Authenticated run attempt")
	f.StringVar(&opts.ArtifactName, "artifact-name", "", "Exact artifact name")
	f.StringVarP(&opts.Output, "output", "o", "", "New extraction directory (must not exist)")
	f.StringVar(&opts.ExpectedSHA, "expected-sha", "", "Expected PR/source head SHA")
	return cmd
}
