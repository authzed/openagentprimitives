package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestEnsureApproval(t *testing.T) {
	cases := []struct {
		name    string
		ctx     func() context.Context
		perm    memory.Permission
		res     string
		wantErr bool
	}{
		{
			name:    "no approval: denied",
			ctx:     context.Background,
			perm:    memory.ReadMemory,
			res:     "nsA/sessA",
			wantErr: true,
		},
		{
			name: "exact bearer match: allowed",
			ctx: func() context.Context {
				return memory.WithApproval(context.Background(),
					memory.ForBearerToken(memory.ReadMemory, "nsA/sessA", "tok-1"))
			},
			perm: memory.ReadMemory, res: "nsA/sessA", wantErr: false,
		},
		{
			name: "wrong perm: denied",
			ctx: func() context.Context {
				return memory.WithApproval(context.Background(),
					memory.ForBearerToken(memory.WriteMemory, "nsA/sessA", "tok-1"))
			},
			perm: memory.ReadMemory, res: "nsA/sessA", wantErr: true,
		},
		{
			name: "wrong resource (cross-session): denied",
			ctx: func() context.Context {
				return memory.WithApproval(context.Background(),
					memory.ForBearerToken(memory.ReadMemory, "nsA/sessA", "tok-1"))
			},
			perm: memory.ReadMemory, res: "nsB/sessB", wantErr: true,
		},
		{
			name: "system approval satisfies any non-internal perm",
			ctx: func() context.Context {
				return memory.WithSystemApproval(context.Background(), "operator:test")
			},
			perm: memory.WriteMemory, res: "any/scope", wantErr: false,
		},
		{
			name: "spicedb source exact match: allowed",
			ctx: func() context.Context {
				return memory.WithApproval(context.Background(),
					memory.ForSpiceDBCheck(memory.ReadMemory, "nsA/sessA", "user:alice"))
			},
			perm: memory.ReadMemory, res: "nsA/sessA", wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := memory.EnsureApproval(tc.ctx(), tc.perm, tc.res)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, memory.ErrMissingApproval), "must wrap ErrMissingApproval")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestWithApproval_Accumulates(t *testing.T) {
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.ReadMemory, "nsA/sessA", "tok-1"))
	ctx = memory.WithApproval(ctx,
		memory.ForBearerToken(memory.WriteMemory, "nsA/sessA", "tok-1"))
	assert.NoError(t, memory.EnsureApproval(ctx, memory.ReadMemory, "nsA/sessA"))
	assert.NoError(t, memory.EnsureApproval(ctx, memory.WriteMemory, "nsA/sessA"))
}
