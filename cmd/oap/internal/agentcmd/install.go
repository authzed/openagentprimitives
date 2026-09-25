package agentcmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// newAgentInstallCmd installs a .oap agent container onto the cluster: it
// loads the bundle (local file/folder, or an OCI ref pulled from a
// registry), fail-closed preflights it, resolves install questions into
// answers + secrets, and applies everything via install.Install.
func newAgentInstallCmd(g *apcmd.Globals) *cobra.Command {
	var (
		name         string
		valuesFile   string
		sets         map[string]string
		verify       bool
		keyPath      string
		plainHTTP    bool
		fitResources bool
		adopt        []string

		buildMissing  bool
		noBuild       bool
		rebuild       bool
		buildSecrets  map[string]string
		buildContexts map[string]string
		platform      string
		vmSSHKey      string
		imageRegistry string
	)

	cmd := &cobra.Command{
		Use:   "install <ref-or-path>",
		Short: "Install a .oap agent container onto the cluster",
		Long: "Install a .oap agent container onto the cluster.\n\n" +
			"<ref-or-path> is either a local .oap file / folder-source directory, or\n" +
			"an OCI reference (\"host[:port]/name[:tag]\") pulled from a registry.\n\n" +
			"--verify checks the ref's cosign-format signature against --key BEFORE\n" +
			"anything is pulled or installed (see `oap agent verify`); a failed check\n" +
			"installs nothing and exits non-zero. --values/--set answer the bundle's\n" +
			"install questions non-interactively; any question still unanswered when\n" +
			"stdin isn't a TTY is a hard error naming it.",
		Args: adoptAwareArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Pure flag parsing with no dependency on the bundle — run it before
			// anything that costs the operator time (a registry pull, the
			// interactive question prompts, an image build) so a mistyped
			// --adopt value fails immediately instead of after all of that.
			adoptAll, adoptKeys, err := splitAdopt(adopt)
			if err != nil {
				return err
			}
			b, src, err := loadInstallSource(cmd, args[0], verify, keyPath, plainHTTP)
			if err != nil {
				return err
			}
			return runCLIInstallGraph(cmd, g, b, src, args[0], cliInstallOptions{
				name: name, valuesFile: valuesFile, sets: sets, fitResources: fitResources,
				adoptAll: adoptAll, adoptKeys: adoptKeys,
				buildMissing: buildMissing, noBuild: noBuild, rebuild: rebuild,
				buildSecrets: buildSecrets, buildContexts: buildContexts,
				platform: platform, vmSSHKey: vmSSHKey, imageRegistry: imageRegistry,
			})
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Install instance name — prefixes every bundled CR's name and is stamped as the install label (default: the bundled AgentClass's own name)")
	cmd.Flags().StringVar(&valuesFile, "values", "", "Path to a YAML file seeding question answers")
	cmd.Flags().StringToStringVar(&sets, "set", nil, "Override a single question's answer (name=value); repeatable")
	cmd.Flags().BoolVar(&verify, "verify", false, "Verify the ref's cosign-format signature against --key before pulling anything (fails closed; ignored for a local path)")
	cmd.Flags().StringVar(&keyPath, "key", "", "Path to a PEM-encoded ECDSA public key (PKIX) to verify against (required with --verify)")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP (no TLS) to reach the registry — for loopback/dev registries")
	cmd.Flags().BoolVar(&buildMissing, "build-missing", false, "Build any referenced image that isn't available, without prompting")
	cmd.Flags().BoolVar(&noBuild, "no-build", false, "Never build; fail if a referenced image isn't available")
	cmd.Flags().BoolVar(&rebuild, "rebuild-images", false, "Rebuild referenced images even if already present")
	cmd.Flags().StringToStringVar(&buildSecrets, "build-secret", nil, "Supply a build secret value (name=env:VAR or name=value); repeatable")
	cmd.Flags().StringToStringVar(&buildContexts, "build-context", nil, "Supply a named build context path (name=/path); repeatable")
	cmd.Flags().StringVar(&platform, "platform", "", "Target platform for built images (default: the cluster's node arch when pushing to a registry, else the docker host arch)")
	cmd.Flags().StringVar(&vmSSHKey, "vm-ssh-key", "", "Path to the oap-desktop VM SSH private key (default: auto-discover; overrides OAP_DESKTOP_SSH_KEY)")
	cmd.Flags().StringVar(&imageRegistry, "image-registry", "", "Registry to push built images to for clusters with no local image-load path (default: inferred from the installed operator image, then from the cluster's cloud). On such a cluster a tag that already exists in the registry is reused as-is and NOT rebuilt, even if the bundle's sources changed — pass --rebuild-images to force a rebuild and push over that tag")
	cmd.Flags().BoolVar(&fitResources, "fit-resources", false, "Answer capacity clamp questions from their suggested defaults instead of prompting (auto-fit to this cluster's ceiling)")
	// The Usage string below intentionally avoids backtick-quoting "oap agent
	// uninstall": pflag's UnquoteUsage reads backtick-delimited text as a
	// type-placeholder override for the flag's value, which would render the
	// --help summary line as "--adopt oap agent uninstall[=all]" instead of
	// "--adopt strings[=all]" — a real defect, not a style choice.
	cmd.Flags().StringSliceVar(&adopt, "adopt", nil, "Adopt a pre-existing object this install would otherwise refuse to overwrite. Bare --adopt adopts every conflicting object except Secrets; --adopt=Kind/Name (repeatable) adopts exactly that object and is the only way to adopt a Secret. An adopted object's spec is overwritten with the bundle's, and oap agent uninstall will delete it.")
	// NoOptDefVal makes the BARE form work; note that pflag then requires the
	// '=' form for a targeted value — adoptAwareArgs rescues the space form
	// with an explanatory error.
	cmd.Flags().Lookup("adopt").NoOptDefVal = adoptAllSentinel
	return cmd
}

// checkSkillsShape refuses a bundle whose AgentClass still carries the
// pre-migration bare-string skills shape, returning nil when it's clean.
// Called from RunE before install.Preflight or install.Install ever touch
// the cluster: left unchecked, the apply would still be refused — but by the
// apiserver's own schema validation, with the opaque "spec.skills[0] in body
// must be of type object: \"string\"" and no hint at the fix. See
// oap.LintSkillsShape for the rewrite this returns instead.
//
// Pulled out of RunE (mirrors skillCloneNotice below) specifically so the
// check-and-format logic is testable on its own — TestCheckSkillsShape
// (install_test.go) exercises it directly, and
// TestAgentInstall_OldSkillsShapeRefusesBeforeAnyClusterWrite
// (install_e2e_test.go) exercises the real RunE end to end against a fake
// cluster, confirming the old shape never reaches an actual apply.
func checkSkillsShape(b *oap.Bundle) error {
	shapeFindings, err := oap.LintSkillsShape(b)
	if err != nil {
		return fmt.Errorf("inspect bundled skills shape: %w", err)
	}
	if len(shapeFindings) == 0 {
		return nil
	}
	var msg strings.Builder
	fmt.Fprintln(&msg, "this bundle's AgentClass still uses the pre-migration skills shape (a bare string) — install refused before touching the cluster:")
	for _, f := range shapeFindings {
		fmt.Fprintln(&msg, f.String())
	}
	return fmt.Errorf("%s", msg.String())
}

// skillCloneNotice renders the operator-facing notice that installing this
// bundle will clone external repositories (its bundled SkillSources). Returns
// the empty string when there are none, so the caller prints nothing.
func skillCloneNotice(clones []oap.SkillClone) string {
	if len(clones) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This agent bundles %d skill source(s); installing will clone these repositories:\n", len(clones))
	for _, c := range clones {
		ref := c.Ref
		if ref == "" {
			ref = "(default branch)"
		}
		fmt.Fprintf(&b, "  - %s @ %s (SkillSource %q)\n", c.RepoURL, ref, c.SkillSource)
	}
	return b.String()
}

// installSourceKind values for installSource.kind — also install.InstallOpts'
// SourceKind, so these constants are the single source of truth for the
// string the AgentClass's oap-source provenance annotation carries.
const (
	installSourceKindRegistry = "registry"
	installSourceKindFile     = "file"
)

// installSource carries the provenance of the bundle Install is about to
// apply: how it was obtained and the digest identifying its exact bytes.
// Always populated (never nil) so install.Install always has a SourceKind to
// stamp — see loadInstallSource.
type installSource struct {
	kind   string // installSourceKindRegistry or installSourceKindFile
	ref    string // the exact ref pulled (may be verify-pinned to a digest); empty for kind==installSourceKindFile
	digest string // the digest identifying the bundle's exact bytes
}

// loadInstallSource resolves <ref-or-path> into a decoded Bundle. A path that
// exists locally (file or folder-source directory, per os.Stat) is loaded via
// loadBundle — the same helper `oap agent inspect`/`oap agent lint` use — and
// its installSource.digest is computed locally (oap.Digest over the packed
// bytes loadBundle already produced) since there is no registry to resolve
// one against. Anything else is treated as an OCI registry ref: pulled via
// oci.Pull, pinned to its verified digest first when --verify is set (mirrors
// `oap agent pull --verify`'s TOCTOU-closed verify-then-pin-then-pull
// sequence), returning the ref pulled + the digest oci.Pull resolved for it.
func loadInstallSource(cmd *cobra.Command, source string, verify bool, keyPath string, plainHTTP bool) (*oap.Bundle, *installSource, error) {
	if _, err := os.Stat(source); err == nil {
		b, packed, err := loadBundle(source)
		if err != nil {
			return nil, nil, err
		}
		dig, err := oap.Digest(packed)
		if err != nil {
			return nil, nil, fmt.Errorf("digest %s: %w", source, err)
		}
		return b, &installSource{kind: installSourceKindFile, digest: dig}, nil
	}

	opts := oci.Options{PlainHTTP: plainHTTP}
	pullRef := source
	if verify {
		if keyPath == "" {
			return nil, nil, fmt.Errorf("--verify requires --key <public-key.pem>")
		}
		pub, err := loadPublicKeyPEM(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("load verification key: %w", err)
		}
		// Verify against the registry BEFORE pulling anything — an unsigned or
		// tampered ref must never reach oci.Pull.
		verifiedDigest, err := oci.Verify(cmd.Context(), source, pub, opts)
		if err != nil {
			return nil, nil, err
		}
		// Pin the pull to the exact digest just verified so a mutable tag can't
		// be re-resolved to different (unsigned) content in between.
		pullRef, err = oci.PinnedRef(source, verifiedDigest)
		if err != nil {
			return nil, nil, fmt.Errorf("pin verified digest for %s: %w", source, err)
		}
	}

	packed, digest, err := oci.Pull(cmd.Context(), pullRef, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("pull %s: %w", pullRef, err)
	}
	b, err := oap.Unpack(packed)
	if err != nil {
		return nil, nil, err
	}
	return b, &installSource{kind: installSourceKindRegistry, ref: pullRef, digest: digest}, nil
}

// installQuestionTitle is the chrome's title bar for every question this
// command asks before the install writes anything. One constant rather than a
// literal per site, so the bundle's own questions and the adopt decision
// cannot title themselves differently.
const installQuestionTitle = "oap · agent install"

// installQuestionPresentation settles how this command asks its screen-based
// questions before the first cluster write: the bundle's manifest questions,
// the capacity clamp, the build-time questions the image reconcile asks, and
// the adopt decision.
//
// Not quite everything: promptRegistry (image_mode.go) still reads os.Stdin
// through a bufio.Scanner of its own, after these have already read over this
// driver's buffered reader. Pre-existing and unchanged here — recorded so the
// claim above is not read as wider than it is.
//
// ONE driver serves all of them, because they read one stdin and the
// line-oriented driver buffers what it reads. A driver apiece would let the
// manifest questions' buffer swallow the line the adopt decision is waiting
// for, and huh's accessible renderer reports that as a completed form with a
// nil error — the adopt question would answer itself with its default and the
// install would proceed on a decision nobody made.
//
// Capabilities come from the stream this command writes to and from its own
// --no-color flag, so a bundle's questions honour the flag every other
// question `oap` asks honours. Re-detecting them somewhere else is how one
// command ends up answering the same flag two ways.
//
// A nil driver is the signal "nobody is at stdin": install.Resolve is then
// given no presentation and never asks, the image reconcile's questions are
// never reached, and the adopt decision is not built at all. Returned as a nil
// interface rather than a nil pointer so the callers' nil checks are true nil
// checks.
//
// No rail. A rail is a claim about where the user is in a fixed sequence, and
// this command has none: how many manifest questions there are depends on the
// bundle, and the adopt decision arrives only if a conflict is found.
func installQuestionPresentation(out io.Writer, in io.Reader, noColor, interactive bool) (tui.Driver, *tui.Theme) {
	if !interactive {
		return nil, nil
	}
	theme := tui.NewTheme(apcmd.DetectCaps(out, noColor))
	return tui.DriverFor(InstallQuestionDriverParams(theme, in, out)), theme
}

// InstallQuestionDriverParams is what this command's driver is built from.
//
// Split out because Inline is not visible in any driver's output, so this is
// the only place a test can read back the decision below.
//
// INLINE, and the rule says so rather than taste: every question here is asked
// from the middle of this command's own output. The bundle's manifest
// questions sit between the skill-clone notice above them and the image
// reconcile below; the build confirm and the build secrets are interleaved with
// per-image progress lines; the adopt decision follows the list of objects it
// is about. An operator answering any of them is reading the stream, and the
// alternate screen would not merely cover it — it makes the terminal's own
// scrollback unreachable for as long as the question is up.
func InstallQuestionDriverParams(theme *tui.Theme, in io.Reader, out io.Writer) tui.DriverParams {
	return tui.DriverParams{
		Theme:  theme,
		Chrome: tui.NewChrome(installQuestionTitle, nil, theme),
		In:     in,
		Out:    out,
		Inline: true,
	}
}
