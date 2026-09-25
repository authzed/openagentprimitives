package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadLLMProvider(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T) string // returns path to pass to loadLLMProvider
		wantNil     bool
		wantErrFrag string // non-empty → expect an error containing this string
	}{
		{
			name: "valid key file: returns non-nil provider",
			setup: func(t *testing.T) string {
				t.Helper()
				f := filepath.Join(t.TempDir(), "key")
				require.NoError(t, os.WriteFile(f, []byte("sk-ant-test-key-12345"), 0600))
				return f
			},
			wantNil: false,
		},
		{
			name: "missing file: returns nil provider, no panic",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			wantNil:     true,
			wantErrFrag: "read key from",
		},
		{
			name: "empty file: returns nil provider, no panic",
			setup: func(t *testing.T) string {
				t.Helper()
				f := filepath.Join(t.TempDir(), "key")
				require.NoError(t, os.WriteFile(f, []byte(""), 0600))
				return f
			},
			wantNil:     true,
			wantErrFrag: "is empty",
		},
		{
			name: "whitespace-only file: returns nil provider (treated as empty)",
			setup: func(t *testing.T) string {
				t.Helper()
				f := filepath.Join(t.TempDir(), "key")
				require.NoError(t, os.WriteFile(f, []byte("   \n"), 0600))
				return f
			},
			wantNil:     true,
			wantErrFrag: "is empty",
		},
		{
			name: "key with trailing newline: strips and returns non-nil provider",
			setup: func(t *testing.T) string {
				t.Helper()
				f := filepath.Join(t.TempDir(), "key")
				require.NoError(t, os.WriteFile(f, []byte("sk-ant-test-key-12345\n"), 0600))
				return f
			},
			wantNil: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			provider, err := loadLLMProvider(path)

			if tc.wantErrFrag != "" {
				require.Error(t, err, "expected an error")
				assert.Contains(t, err.Error(), tc.wantErrFrag)
			} else {
				require.NoError(t, err)
			}

			if tc.wantNil {
				assert.Nil(t, provider, "provider must be nil when key is absent or empty")
			} else {
				assert.NotNil(t, provider, "provider must be non-nil for a valid key")
				assert.Equal(t, "anthropic", provider.Name())
			}
		})
	}
}
