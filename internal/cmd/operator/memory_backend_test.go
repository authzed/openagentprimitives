package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

// TestValidateMemoryBackend covers the fail-closed MEMORY_BACKEND selector: an
// unset selector is a fatal configuration error (no silent inmem fallback),
// postgres requires POSTGRES_URI, sqlite requires MEMORY_SQLITE_PATH, and an
// unknown value is rejected loudly.
func TestValidateMemoryBackend(t *testing.T) {
	cases := []struct {
		name        string
		backend     string
		postgresURI string
		sqlitePath  string // set via t.Setenv(memsqlite.EnvPath, ...) when non-empty
		wantKind    string
		wantErr     string // substring; "" means no error expected
	}{
		{
			name:    "empty: fatal, no silent default",
			backend: "",
			wantErr: "MEMORY_BACKEND must be set",
		},
		{
			name:     "inmem: ok (pure in-memory)",
			backend:  "inmem",
			wantKind: memoryBackendInmem,
		},
		{
			name:        "inmem ignores POSTGRES_URI",
			backend:     "inmem",
			postgresURI: "postgres://x",
			wantKind:    memoryBackendInmem,
		},
		{
			name:        "postgres with URI: ok",
			backend:     "postgres",
			postgresURI: "postgres://user:pw@host:5432/db",
			wantKind:    memoryBackendPostgres,
		},
		{
			name:    "postgres without URI: fatal (requires POSTGRES_URI)",
			backend: "postgres",
			wantErr: "MEMORY_BACKEND=postgres requires POSTGRES_URI",
		},
		{
			name:       "sqlite with MEMORY_SQLITE_PATH: ok",
			backend:    "sqlite",
			sqlitePath: "/tmp/ap-memory-test.db",
			wantKind:   memoryBackendSqlite,
		},
		{
			name:    "sqlite without MEMORY_SQLITE_PATH: fatal (requires MEMORY_SQLITE_PATH)",
			backend: "sqlite",
			wantErr: "MEMORY_BACKEND=sqlite requires MEMORY_SQLITE_PATH",
		},
		{
			name:    "bogus value: fatal (rejected loudly)",
			backend: "mysql",
			wantErr: `unknown MEMORY_BACKEND "mysql"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sqlitePath != "" {
				t.Setenv(memsqlite.EnvPath, tc.sqlitePath)
			}
			kind, err := validateMemoryBackend(tc.backend, tc.postgresURI)
			if tc.wantErr != "" {
				require.Error(t, err, "selector must fail closed")
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, kind, "no backend kind on error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantKind, kind)
		})
	}
}

// TestInmemBackendConstructs asserts the inmem selector's construction path
// yields a usable Backend (the postgres connect path is not unit-testable, so
// the validation above covers its fail-closed rules).
func TestInmemBackendConstructs(t *testing.T) {
	kind, err := validateMemoryBackend(memoryBackendInmem, "")
	require.NoError(t, err)
	require.Equal(t, memoryBackendInmem, kind)
	assert.NotNil(t, memoryinmem.NewBackend(), "inmem backend must construct")
}
