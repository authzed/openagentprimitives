package gke

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// EnsureRegistryReady prepares a GKE Artifact Registry repository for
// pushing: it wires Docker authentication (gcloud auth configure-docker)
// and ensures the target repository exists (creating it if needed). For
// non-Artifact-Registry hosts it is a no-op. This mirrors the cmd/oap
// ensureRegistryReady/ensureGKEArtifactRegistry logic but belongs in
// pkg/platform/cloud/gke so cmd/oap can dispatch through the Strategy interface.
//
// createRegistry forces repository creation without prompting (for
// non-interactive runs); interactive runs offer a Y/n via confirmFn.
func EnsureRegistryReady(ctx context.Context, rep cloud.Reporter, registry string, interactive, createRegistry bool, confirmFn func(string) bool) error {
	host := RegistryHost(registry)
	if !IsGKEArtifactRegistry(host) {
		return nil
	}
	return ensureGKEArtifactRegistry(ctx, rep, registry, interactive, createRegistry, confirmFn)
}

// RegistryHost returns the host segment of a registry URL (the part
// before the first '/').
func RegistryHost(registry string) string {
	if i := strings.Index(registry, "/"); i >= 0 {
		return registry[:i]
	}
	return registry
}

// IsGKEArtifactRegistry reports whether host ends with the Artifact
// Registry suffix (-docker.pkg.dev).
func IsGKEArtifactRegistry(host string) bool {
	return strings.HasSuffix(host, artifactRegistrySuffix)
}

// arRegistry is a parsed GKE Artifact Registry reference.
type arRegistry struct{ host, location, project, repo string }

// parseARRegistry parses <region>-docker.pkg.dev/<project>/<repo>[/...].
// The repo is the first path segment after the project (AR repos are
// single-level). Returns ok=false for non-AR hosts or too-few segments.
func parseARRegistry(registry string) (arRegistry, bool) {
	parts := strings.SplitN(registry, "/", 3)
	if len(parts) < 3 {
		return arRegistry{}, false
	}
	host := parts[0]
	if !IsGKEArtifactRegistry(host) || parts[1] == "" || parts[2] == "" {
		return arRegistry{}, false
	}
	return arRegistry{
		host:     host,
		location: strings.TrimSuffix(host, artifactRegistrySuffix),
		project:  parts[1],
		repo:     strings.SplitN(parts[2], "/", 2)[0],
	}, true
}

func ensureGKEArtifactRegistry(ctx context.Context, rep cloud.Reporter, registry string, interactive, createRegistry bool, confirmFn func(string) bool) error {
	ar, ok := parseARRegistry(registry)
	if !ok {
		return nil
	}

	// 1. Docker auth — idempotent; skip the gcloud call if already configured.
	if dockerConfiguredForHost(ar.host) {
		rep.Info("  ✓ docker auth configured for %s", ar.host)
	} else {
		rep.Step("  configuring docker auth for %s", ar.host)
		if _, err := cloud.Gcloud(ctx, rep, "auth", "configure-docker", ar.host, "--quiet"); err != nil {
			return fmt.Errorf("configure docker auth for %s (is the gcloud CLI installed and authenticated?): %w", ar.host, err)
		}
	}

	// 2. Repository existence.
	exists, err := arRepoExists(ctx, rep, ar)
	if err != nil {
		return err
	}
	if exists {
		rep.Info("  ✓ repository %s exists", ar.repo)
		return nil
	}
	createCmd := fmt.Sprintf(
		"gcloud artifacts repositories create %s --repository-format=docker --location=%s --project=%s",
		ar.repo, ar.location, ar.project)
	if !createRegistry {
		if !interactive {
			return fmt.Errorf(
				"artifact registry repository %q not found in project %s (%s) — re-run with --create-registry, or create it:\n  %s",
				ar.repo, ar.project, ar.location, createCmd)
		}
		if confirmFn == nil || !confirmFn(fmt.Sprintf("  repository %q not found — create it now?", ar.repo)) {
			return fmt.Errorf("repository %q does not exist; create it (%s) or pass --create-registry", ar.repo, createCmd)
		}
	}
	rep.Step("  creating repository %s (%s)…", ar.repo, ar.location)
	if _, err := cloud.Gcloud(ctx, rep, "artifacts", "repositories", "create", ar.repo,
		"--repository-format=docker", "--location="+ar.location, "--project="+ar.project); err != nil {
		return fmt.Errorf("create artifact registry repository %q: %w", ar.repo, err)
	}
	rep.OK("  ✓ created repository %s", ar.repo)
	return nil
}

// dockerConfiguredForHost reports whether ~/.docker/config.json registers
// a credential helper for host, so `docker push` can authenticate to it.
func dockerConfiguredForHost(host string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
	if err != nil {
		return false
	}
	return CredHelperConfigured(data, host)
}

// CredHelperConfigured reports whether a Docker config.json byte slice
// names a credential helper for host. Exported for testability.
func CredHelperConfigured(configJSON []byte, host string) bool {
	var cfg struct {
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if json.Unmarshal(configJSON, &cfg) != nil {
		return false
	}
	return cfg.CredHelpers[host] != ""
}

// arRepoExists reports whether the Artifact Registry repository exists.
// Uses `repositories list` (not `describe`) because gcloud's describe
// crashes formatting an empty repository's size and cannot be used as
// an existence probe.
func arRepoExists(ctx context.Context, rep cloud.Reporter, ar arRegistry) (bool, error) {
	out, err := cloud.Gcloud(ctx, rep,
		"artifacts", "repositories", "list",
		"--location="+ar.location, "--project="+ar.project, "--format=value(name.basename())")
	if err != nil {
		return false, fmt.Errorf("list artifact registry repositories in %s/%s: %w", ar.project, ar.location, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == ar.repo {
			return true, nil
		}
	}
	return false, nil
}
