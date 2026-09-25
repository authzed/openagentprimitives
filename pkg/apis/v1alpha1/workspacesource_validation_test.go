package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateWorkspaceSourceSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    WorkspaceSourceSpec
		wantErr string
	}{
		{
			name: "valid: kind+locator, no scope → ok",
			spec: WorkspaceSourceSpec{Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"}},
		},
		{
			name:    "missing kind → error",
			spec:    WorkspaceSourceSpec{Source: WorkspaceSourceRef{Locator: "https://example.com/o/r.git"}},
			wantErr: "source.kind is required",
		},
		{
			name:    "missing locator → error",
			spec:    WorkspaceSourceSpec{Source: WorkspaceSourceRef{Kind: "git"}},
			wantErr: "source.locator is required",
		},
		{
			name: "absolute scope path → error",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Scope:  WorkspaceScope{Paths: []WorkspaceScopePath{{Path: "/etc"}}},
			},
			wantErr: "must be relative",
		},
		{
			name: "bad base size → error",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Base:   WorkspaceBase{Size: "not-a-quantity"},
			},
			wantErr: "base.size",
		},
		{
			name: "base refresh onDemand → ok",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Base:   WorkspaceBase{Refresh: "onDemand"},
			},
		},
		{
			name: "base refresh valid duration → ok",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Base:   WorkspaceBase{Refresh: "10m"},
			},
		},
		{
			name: "base refresh at the 1m floor → ok",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Base:   WorkspaceBase{Refresh: "1m"},
			},
		},
		{
			name: "base refresh not onDemand or a duration → error",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Base:   WorkspaceBase{Refresh: "not-a-duration"},
			},
			wantErr: "base.refresh",
		},
		{
			name: "base refresh below 1m floor (30s) → error",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Base:   WorkspaceBase{Refresh: "30s"},
			},
			wantErr: "base.refresh",
		},
		{
			name: "base refresh 0s → error",
			spec: WorkspaceSourceSpec{
				Source: WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git"},
				Base:   WorkspaceBase{Refresh: "0s"},
			},
			wantErr: "base.refresh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWorkspaceSourceSpec("demo-source", tc.spec)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
