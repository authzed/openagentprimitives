package postgres

import (
	"fmt"
	"os"
)

const EnvURI = "POSTGRES_URI"

type EnvConfig struct {
	URI string
}

func LoadEnvConfig() (EnvConfig, error) {
	cfg := EnvConfig{
		URI: os.Getenv(EnvURI),
	}
	if cfg.URI == "" {
		return cfg, fmt.Errorf("postgres: required env var not set: %s", EnvURI)
	}
	return cfg, nil
}
