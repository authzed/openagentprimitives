package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"

	// The channel kinds LintRequiredChannels resolves a declared kind against.
	// Declared here rather than borrowed from channelcmd's copy: this package
	// is what needs a populated registry, and its own test binary links only
	// what it imports. LintRequiredChannels refuses to run against an empty
	// registry rather than calling every declared kind unregistered.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func newAgentLintCmd(_ *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "lint <path-to-oap-or-folder>",
		Short: "Validate an agent container (.oap or folder source): manifest, questions, binding targets, and any bundled AgentUI",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, _, err := loadBundle(args[0])
			if err != nil {
				return err
			}
			if err := b.Validate(); err != nil {
				return fmt.Errorf("lint failed: %w", err)
			}
			// Validate checks manifest, questions, and that each binding target's
			// Kind/Name resolves to a bundled CR — but NOT that the target field
			// PATH exists on that CR. Keep the success message honest about that
			// scope (full path resolution is a follow-up).
			fmt.Fprintf(cmd.OutOrStdout(), "OK — %s v%s: manifest, questions, and binding targets (Kind/Name) are structurally valid\n", b.Manifest.Agent.Name, b.Manifest.Agent.Version)

			hardFail := false

			// LintSkillsShape runs FIRST and gates every check below —
			// LintAgentUIs, LintSkillPinning, and LintArtifactsCapability all
			// decode each bundled AgentClass strictly into the typed AgentSkill
			// shape (via decodeBundledCR), and that decode fails on a
			// pre-migration bare-string skills entry with a raw "cannot unmarshal
			// string into Go struct field" error that names no fix — the exact
			// failure this check exists to pre-empt. Confirmed against
			// LintAgentUIs specifically: with this check running after it
			// instead of before, `oap agent lint` on an old-shape bundle reported
			// only "agent-UI lint failed: ... cannot unmarshal string ..." and
			// never reached this check at all (pinned automatically now by
			// TestAgentLint_OldSkillsShapeLeadsWithTheRewrite in lint_test.go).
			shapeFindings, err := oap.LintSkillsShape(b)
			if err != nil {
				return fmt.Errorf("skills-shape lint failed: %w", err)
			}
			if len(shapeFindings) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no skills-shape findings")
			}
			for _, f := range shapeFindings {
				fmt.Fprintln(cmd.OutOrStdout(), f.String())
				hardFail = true
			}
			if len(shapeFindings) > 0 {
				return fmt.Errorf("lint found problems an author must fix before this bundle can be installed")
			}

			// oap.LintAgentUIs is deliberately a SEPARATE, advisory check, not
			// folded into Bundle.Validate: Validate also gates install.Preflight,
			// install.apply, source.OpenCluster, and the desktop installer, and
			// an author's AgentUI mistake must not turn into a failed *install* —
			// nor is the bundle's own eligibleTools ceiling necessarily the whole
			// picture on a target cluster, where another AgentClass in the
			// namespace could grant more. Only `oap agent lint` calls it.
			uiFindings, err := oap.LintAgentUIs(b)
			if err != nil {
				return fmt.Errorf("agent-UI lint failed: %w", err)
			}
			if len(uiFindings) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no agent-UI findings")
			}
			// Every finding prints, informational or not — silence about a
			// finding the lint found is indistinguishable from not checking at
			// all. Only a non-informational finding fails the command: an
			// AgentUI naming an origin the bundle does not carry is expected
			// to be resolved by whatever the target cluster already has
			// installed, not by this bundle's own contents.
			for _, f := range uiFindings {
				fmt.Fprintln(cmd.OutOrStdout(), f.String())
				if !f.Informational {
					hardFail = true
				}
			}

			// The two bundle-wide requirement checks below are, like
			// LintAgentUIs, advisory rather than folded into Bundle.Validate — an
			// unpinned skill or a missing artifacts capability is an authoring
			// mistake, not something an install should refuse over. Unlike
			// Finding, RequirementFinding carries no informational case: both
			// checks exist specifically because their failure mode is a SILENT
			// quality defect (a rolling methodology reference, a tool nobody can
			// call) rather than a "the rest is on the target cluster" shape, so
			// every RequirementFinding here fails the command.
			pinFindings, err := oap.LintSkillPinning(b)
			if err != nil {
				return fmt.Errorf("skill-pinning lint failed: %w", err)
			}
			if len(pinFindings) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no skill-pinning findings")
			}
			for _, f := range pinFindings {
				fmt.Fprintln(cmd.OutOrStdout(), f.String())
				hardFail = true
			}

			artifactFindings, err := oap.LintArtifactsCapability(b)
			if err != nil {
				return fmt.Errorf("artifacts-capability lint failed: %w", err)
			}
			if len(artifactFindings) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no artifacts-capability findings")
			}
			for _, f := range artifactFindings {
				fmt.Fprintln(cmd.OutOrStdout(), f.String())
				hardFail = true
			}

			// The required-channels check lives in oap/channelplan rather than
			// in oap: it needs the channel-kind registry, and channelkinds
			// imports oap, so oap importing the registry back would be a cycle.
			// Its findings fail the command like the two above — a declaration
			// naming a kind this build does not have, or an AgentIdentity
			// pointing at a credentials Secret no declared channel produces,
			// installs clean and then fails at the first inbound with no token.
			channelFindings, err := channelplan.LintRequiredChannels(b)
			if err != nil {
				return fmt.Errorf("required-channels lint failed: %w", err)
			}
			if len(channelFindings) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no required-channel findings")
			}
			for _, f := range channelFindings {
				fmt.Fprintln(cmd.OutOrStdout(), f.String())
				hardFail = true
			}

			if hardFail {
				return fmt.Errorf("lint found problems an author must fix before this bundle can be installed")
			}
			return nil
		},
	}
}
