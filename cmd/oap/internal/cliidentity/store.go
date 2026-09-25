// Package cliidentity caches the identityd-issued identity assertion
// for the local user. The file holds the signed assertion plus the
// display claims identityd returned — oap cannot verify the HMAC (it
// holds no key) and does not need to: identityd verified the login
// and in-cluster consumers verify the signature.
package cliidentity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Cached holds the identityd-issued assertion and associated claims.
type Cached struct {
	Assertion   string `json:"assertion"`
	Subject     string `json:"subject"` // canonical, no "user:" prefix
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	ExpiresAt   int64  `json:"expiresAt"` // Unix seconds
}

// ConfigDirFn is the function that returns the base config directory.
// Production code uses os.UserConfigDir. Tests override this variable to
// redirect writes to a temp directory (and restore it in t.Cleanup).
//
// Usage in tests:
//
//	orig := cliidentity.ConfigDirFn
//	cliidentity.ConfigDirFn = func() (string, error) { return t.TempDir(), nil }
//	t.Cleanup(func() { cliidentity.ConfigDirFn = orig })
var ConfigDirFn = os.UserConfigDir

// Path returns the absolute path to the identity cache file:
// filepath.Join(ConfigDirFn(), "agentprimitives", "identity.json").
func Path() (string, error) {
	base, err := ConfigDirFn()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agentprimitives", "identity.json"), nil
}

// Save writes c to the cache file. The parent directory is created with
// mode 0700 if absent; the file is written with mode 0600.
func Save(c Cached) error {
	p, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}

// Load returns the cached identity. Returns (nil, nil) when the file is
// absent. Returns (nil, nil) and deletes the file when the cached entry
// has expired (ExpiresAt <= now).
func Load() (*Cached, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var c Cached
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.ExpiresAt <= time.Now().Unix() {
		// Expired: delete and return cache miss so the caller re-authenticates.
		_ = os.Remove(p)
		return nil, nil
	}
	return &c, nil
}

// Delete removes the cache file. A missing file is not an error.
func Delete() error {
	p, err := Path()
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
