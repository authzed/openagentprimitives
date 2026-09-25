package sqlite

import (
	"fmt"
	"os"
)

// EnvPath is the env var naming the on-disk SQLite database file. When
// set, the operator selects the SQLite memory backend (see the backend
// precedence in internal/cmd/operator/main.go).
const EnvPath = "MEMORY_SQLITE_PATH"

type EnvConfig struct {
	Path string
}

func LoadEnvConfig() (EnvConfig, error) {
	cfg := EnvConfig{Path: os.Getenv(EnvPath)}
	if cfg.Path == "" {
		return cfg, fmt.Errorf("sqlite: required env var not set: %s", EnvPath)
	}
	return cfg, nil
}
