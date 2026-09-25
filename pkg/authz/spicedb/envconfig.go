package spicedb

import (
	"fmt"
	"os"
	"strings"
)

// Canonical env-var names every binary in this project uses to find
// SpiceDB. Defined here so the operator, runner, channelsd, and the
// agentsession podspec all agree — drift in the names is exactly how
// "per-tool authz silently disabled" footguns happen.
const (
	EnvEndpoint = "SPICEDB_ENDPOINT"
	EnvToken    = "SPICEDB_TOKEN"
	EnvInsecure = "SPICEDB_INSECURE"
	// EnvTokenPath, when set, names a file the preshared token is read
	// from instead of carrying it in EnvToken. It takes precedence over
	// EnvToken. Runner pods use this so the token rides in a mounted
	// Secret file rather than as a plaintext env var.
	EnvTokenPath = "SPICEDB_TOKEN_PATH"
)

// Canonical install-time resource names that hold the SpiceDB endpoint
// (in a ConfigMap) and the gRPC preshared token (in a Secret). The
// operator reads these resources at startup (via LoadEnvConfig) to
// resolve its own connection parameters. SPICEDB_ENDPOINT and
// SPICEDB_INSECURE are then value-copied onto each runner pod's env
// (cross-namespace boundary: runner pods live in the session namespace,
// not agentprimitives-system). The preshared token is NOT value-copied
// as an env var: the controller writes it into the per-session Secret
// under key "spicedb-token", mounts it as a file, and runner pods read
// it via SPICEDB_TOKEN_PATH — never as a plaintext env var.
const (
	SharedConfigMapName        = "spicebox-spicedb-config"
	SharedConfigMapEndpointKey = "endpoint"
	SharedTokenSecretName      = "spicebox-spicedb-token"
	SharedTokenSecretKey       = "token"
)

// EnvConfig is the resolved set of SpiceDB connection parameters
// loaded from the process environment.
type EnvConfig struct {
	Endpoint string
	Token    string
	Insecure bool
}

// LoadEnvConfig reads SPICEDB_ENDPOINT, SPICEDB_TOKEN,
// SPICEDB_TOKEN_PATH, and SPICEDB_INSECURE from the environment and
// returns the resolved config. Endpoint and the resolved Token are both
// REQUIRED — a missing value returns an error naming every missing var
// so callers can surface it loudly. Binaries that talk to SpiceDB
// (operator, runner) use this at startup and refuse to run on error,
// which is what prevents silent per-tool-authz bypass.
//
// When SPICEDB_TOKEN_PATH is set and non-empty the token is read from
// that file (trimmed of surrounding whitespace) and takes precedence
// over SPICEDB_TOKEN; a file read error is returned wrapped. This lets
// runner pods carry the preshared token as a mounted Secret file rather
// than a plaintext env var. SPICEDB_TOKEN remains the fallback when no
// path is set.
func LoadEnvConfig() (EnvConfig, error) {
	cfg := EnvConfig{
		Endpoint: os.Getenv(EnvEndpoint),
		Token:    os.Getenv(EnvToken),
		Insecure: os.Getenv(EnvInsecure) == "true",
	}
	tokenPath := os.Getenv(EnvTokenPath)
	if tokenPath != "" {
		raw, err := os.ReadFile(tokenPath)
		if err != nil {
			return cfg, fmt.Errorf("spicedb: read %s file %q: %w", EnvTokenPath, tokenPath, err)
		}
		cfg.Token = strings.TrimSpace(string(raw))
	}
	var missing []string
	if cfg.Endpoint == "" {
		missing = append(missing, EnvEndpoint)
	}
	if cfg.Token == "" {
		if tokenPath != "" {
			// A path was given but the file was empty or whitespace-only.
			// Report any other missing vars first so the operator sees the
			// full picture, then emit the focused token-file error.
			if len(missing) > 0 {
				return cfg, fmt.Errorf(
					"spicedb: required env var(s) not set: %s — wire %s ConfigMap %q into the pod; "+
						"additionally, token file %q (from %s) is empty or whitespace-only — ensure the per-session Secret carries a non-empty %q key",
					strings.Join(missing, ", "),
					EnvEndpoint, SharedConfigMapName,
					tokenPath, EnvTokenPath, SharedTokenSecretKey,
				)
			}
			// Endpoint is fine; give a precise, actionable token-file error.
			// Do not mention SPICEDB_TOKEN — runner pods use SPICEDB_TOKEN_PATH.
			return cfg, fmt.Errorf(
				"spicedb: token file %q (from %s) is empty or whitespace-only — ensure the per-session Secret carries a non-empty %q key",
				tokenPath, EnvTokenPath, SharedTokenSecretKey,
			)
		}
		missing = append(missing, EnvToken)
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf(
			"spicedb: required env var(s) not set: %s — wire %s ConfigMap %q and %s Secret %q into the pod",
			strings.Join(missing, ", "),
			EnvEndpoint, SharedConfigMapName,
			EnvToken, SharedTokenSecretName,
		)
	}
	return cfg, nil
}
