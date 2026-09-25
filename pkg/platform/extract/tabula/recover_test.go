package tabula

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// TestRecoverToError proves the recover mechanism itself, independent of
// any particular hostile tabula fixture: this is the only thing standing
// between an untrusted upload and a crashed pod, so it needs a test that
// doesn't depend on finding a real library panic trigger.
func TestRecoverToError(t *testing.T) {
	cases := []struct {
		name       string
		fn         func() (extract.Result, error)
		wantResult extract.Result
		wantErrMsg string // substring; empty means no error expected
	}{
		{
			name: "panic: converted to a clean error and a zero Result, not a crash",
			fn: func() (extract.Result, error) {
				panic("synthetic panic for hostile-input recovery test")
			},
			wantResult: extract.Result{},
			wantErrMsg: "synthetic panic for hostile-input recovery test",
		},
		{
			name: "normal return: passes through untouched",
			fn: func() (extract.Result, error) {
				return extract.Result{Text: "ok", Pages: 3}, nil
			},
			wantResult: extract.Result{Text: "ok", Pages: 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := recoverToError("application/x-test", tc.fn)

			if tc.wantErrMsg == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a panic inside fn must surface as an error, not crash the test process")
				assert.Contains(t, err.Error(), tc.wantErrMsg)
			}
			assert.Equal(t, tc.wantResult, result)
		})
	}
}
