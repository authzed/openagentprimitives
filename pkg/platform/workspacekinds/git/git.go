// Package git implements the git workspace-source driver: it builds the git
// commands to materialize a shared read-cache base, sync an overlay to the
// latest revision, and (Phase 4) apply overlay commits back to the origin.
// Importing this package side-effects its registration into
// pkg/platform/workspacekinds/registry.
package git

import (
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

func init() { registry.Register(New()) }

// Kind implements workspacekinds.Kind for git sources.
type Kind struct{}

// Compile-time interface satisfaction checks.
var (
	_ workspacekinds.Kind                = Kind{}
	_ workspacekinds.Applier             = Kind{}
	_ workspacekinds.CredentialedApplier = Kind{}
)

// gitApplyTokenEnv is the env var ApplyCommands' inline credential helper
// reads the push token from; the framework injects it via secretKeyRef, never
// onto argv. gitApplyCredentialKey is the key of that token within the
// session's credential Secret — it matches the "git-token" credential the git
// toolkit declares (toolkits/git.yaml).
const (
	gitApplyTokenEnv      = "WORKSPACE_GIT_TOKEN"
	gitApplyCredentialKey = "git-token"
)

// New returns the git workspace-source driver.
func New() *Kind { return &Kind{} }

// Name is the registry key.
func (Kind) Name() string { return "git" }

// Validate checks the locator is present and every scope path is a safe
// relative subpath (no absolute, no "..", no home reference, no backslash, no
// leading/trailing whitespace). It also rejects a dash-leading locator or ref:
// both are interpolated into a git argv, and a value that looks like an option
// is never a legitimate URL or ref name — refusing it here keeps the guarantee
// at the spec boundary rather than resting on how git happens to consume the
// token (CWE-88, argument injection).
func (Kind) Validate(spec workspacekinds.Spec) error {
	var problems []string
	if strings.TrimSpace(spec.Locator) == "" {
		problems = append(problems, "locator is required")
	}
	if strings.HasPrefix(spec.Locator, "-") {
		problems = append(problems, "locator must not begin with '-' (would read as a git option)")
	}
	if strings.HasPrefix(spec.Ref, "-") {
		problems = append(problems, "ref must not begin with '-' (would read as a git option)")
	}
	for i, p := range spec.Scope.Paths {
		if err := workspacekinds.ValidateScopePath(p.Path); err != nil {
			problems = append(problems, fmt.Sprintf("scope.paths[%d]: %v", i, err))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems) // stable message for status conditions
	return fmt.Errorf("git workspace source invalid: %s", strings.Join(problems, "; "))
}

// gitEnv returns a hermetic environment for read git operations. It carries
// only PATH (so git can find its transport helpers) plus lockdown vars: no
// system/global/user git config (which is where credential helpers and
// url.insteadOf tokens live), interactive prompts disabled, HOME pointed at a
// nonexistent dir, and the transport protocol allowlist restricted to
// file/http/https (blocking ext::/fd:: local-exec transports). Everything else
// in the ambient environment — including any inherited push token — is dropped.
func gitEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/nonexistent",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ALLOW_PROTOCOL=file:http:https",
	}
}

// MaterializeCommands builds a git clone that populates destDir from the source.
// --end-of-options terminates option parsing so a hostile locator cannot inject
// a flag (--upload-pack=<cmd> being the classic: CVE-2019-13139). It is
// preferred over "--" because git overloads "--" per subcommand — for the
// rev/pathspec commands it starts PATHSPECS rather than ending options — so
// --end-of-options is the one terminator that means the same thing everywhere
// (git 2.24+; the materialize image is alpine/git).
func (k Kind) MaterializeCommands(spec workspacekinds.Spec, destDir string) ([]workspacekinds.Command, error) {
	if err := k.Validate(spec); err != nil {
		return nil, err
	}
	argv := []string{"git", "clone"}
	if spec.Ref != "" {
		argv = append(argv, "--branch", spec.Ref)
	}
	argv = append(argv, "--end-of-options", spec.Locator, destDir)
	return []workspacekinds.Command{{Env: gitEnv(), Argv: argv}}, nil
}

// SyncCommands builds a fast-forward-only pull that updates workDir to the
// latest source revision. --ff-only means a divergent upstream surfaces as an
// error rather than a silent merge that could rewrite the overlay.
//
// Assumes the workDir is on a branch (as a branch-ref materialize produces); a
// detached HEAD — e.g. from a tag materialize — will error on pull rather than
// silently no-op.
func (k Kind) SyncCommands(spec workspacekinds.Spec, workDir string) ([]workspacekinds.Command, error) {
	if err := k.Validate(spec); err != nil {
		return nil, err
	}
	return []workspacekinds.Command{{
		Dir:  workDir,
		Env:  gitEnv(),
		Argv: []string{"git", "pull", "--ff-only"},
	}}, nil
}

// ApplyCredential declares the env var ApplyCommands' credential helper reads
// the push token from, and the Secret key (in the session credential Secret)
// that holds it. The framework injects env[envVar] <- secretKeyRef(key) into
// the reconcile Job; the token never lands on argv.
func (k Kind) ApplyCredential() (string, string) { return gitApplyTokenEnv, gitApplyCredentialKey }

// ApplyCommands pushes the overlay's current HEAD to the source branch. The
// scoped push credential is injected by the framework as the
// WORKSPACE_GIT_TOKEN env var (never onto argv or into a file); an inline
// `-c credential.helper=` shell function reads it from the environment at
// push time using the GitHub HTTPS convention (any username, token as
// password). Other git hosts may need a different helper shape — a follow-up.
func (k Kind) ApplyCommands(spec workspacekinds.Spec, workDir string) ([]workspacekinds.Command, error) {
	if err := k.Validate(spec); err != nil {
		return nil, err
	}
	dst := "HEAD"
	if spec.Ref != "" {
		dst = "HEAD:" + spec.Ref
	}
	// Inline credential helper: git runs it (sh) on auth, reading the token from
	// the WORKSPACE_GIT_TOKEN env the framework injects. The token never appears
	// on argv. GitHub HTTPS: any username + token-as-password; other hosts TBD.
	helper := `!f() { echo username=x-access-token; echo "password=$` + gitApplyTokenEnv + `"; }; f`
	// --end-of-options for the same reason as MaterializeCommands: dst embeds
	// spec.Ref. It is "HEAD"-prefixed today so it cannot lead with a dash, but
	// the terminator makes that a property of the argv rather than of the
	// refspec's current shape.
	return []workspacekinds.Command{{
		Dir:  workDir,
		Env:  gitEnv(),
		Argv: []string{"git", "-c", "credential.helper=", "-c", "credential.helper=" + helper, "push", "--end-of-options", "origin", dst},
	}}, nil
}
