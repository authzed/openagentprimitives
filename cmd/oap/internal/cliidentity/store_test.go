package cliidentity

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTempConfigDir overrides ConfigDirFn for the duration of the test.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := ConfigDirFn
	ConfigDirFn = func() (string, error) { return dir, nil }
	t.Cleanup(func() { ConfigDirFn = orig })
	return dir
}

func TestSaveLoad_Roundtrip(t *testing.T) {
	withTempConfigDir(t)

	c := Cached{
		Assertion:   "a.b.c",
		Subject:     "YWxpY2VAZXhhbXBsZS5jb20",
		Email:       "alice@example.com",
		DisplayName: "Alice",
		ExpiresAt:   time.Now().Add(12 * time.Hour).Unix(),
	}
	require.NoError(t, Save(c))

	got, err := Load()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, c, *got)
}

func TestLoad_Missing_ReturnsNil(t *testing.T) {
	withTempConfigDir(t)

	got, err := Load()
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLoad_Expired_ReturnsNilAndDeletesFile(t *testing.T) {
	withTempConfigDir(t)

	c := Cached{
		Assertion: "x.y.z",
		Email:     "alice@example.com",
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(), // expired
	}
	require.NoError(t, Save(c))

	// Confirm file exists before Load.
	p, err := Path()
	require.NoError(t, err)
	_, err = os.Stat(p)
	require.NoError(t, err, "file should exist before Load")

	got, err := Load()
	require.NoError(t, err)
	assert.Nil(t, got, "expired entry should return nil")

	// File must be gone after Load.
	_, err = os.Stat(p)
	assert.True(t, os.IsNotExist(err), "expired file should be deleted")
}

func TestSave_FileAndDirPerms(t *testing.T) {
	base := withTempConfigDir(t)

	c := Cached{
		Email:     "alice@example.com",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	}
	require.NoError(t, Save(c))

	p, err := Path()
	require.NoError(t, err)

	// File must be 0600.
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm(), "cache file must be 0600")

	// Directory must be 0700.
	dir := filepath.Join(base, "agentprimitives")
	di, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0700), di.Mode().Perm(), "cache directory must be 0700")
}

func TestDelete_Missing_NoError(t *testing.T) {
	withTempConfigDir(t)

	// File doesn't exist; Delete must not error.
	require.NoError(t, Delete())
}

func TestDelete_RemovesFile(t *testing.T) {
	withTempConfigDir(t)

	c := Cached{
		Email:     "alice@example.com",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	}
	require.NoError(t, Save(c))

	require.NoError(t, Delete())

	p, err := Path()
	require.NoError(t, err)
	_, err = os.Stat(p)
	assert.True(t, os.IsNotExist(err), "file should be gone after Delete")
}
