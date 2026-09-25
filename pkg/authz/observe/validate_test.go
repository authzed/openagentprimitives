package observe_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/observe"
)

func TestValidateBlock(t *testing.T) {
	valid := observe.Block{
		Subjects: []observe.SubjectExpr{
			{ResourceType: `"github_pr"`, ResourceID: `item.headRepository.nameWithOwner + "#" + string(item.number)`},
		},
		Facts: map[string]string{"is_cross_repository": "item.isCrossRepository"},
	}

	cases := []struct {
		name    string
		mutate  func(b *observe.Block)
		wantErr string
	}{
		{
			name:   "valid block compiles",
			mutate: func(*observe.Block) {},
		},
		{
			name:    "facts with no subject: rejected, because a subjectless fact is a session-scoped boolean",
			mutate:  func(b *observe.Block) { b.Subjects = nil },
			wantErr: "at least one subject",
		},
		{
			name:    "no facts: rejected, because the block would record nothing",
			mutate:  func(b *observe.Block) { b.Facts = nil },
			wantErr: "at least one fact",
		},
		{
			name: "uncompilable subject expression: rejected at admission, not at dispatch",
			mutate: func(b *observe.Block) {
				b.Subjects[0].ResourceID = `item.number +` // syntax error
			},
			wantErr: "CEL compile",
		},
		{
			name: "uncompilable fact expression: rejected",
			mutate: func(b *observe.Block) {
				b.Facts["is_cross_repository"] = `nosuchfunc(item)`
			},
			wantErr: "CEL compile",
		},
		{
			name:    "empty fact name: rejected",
			mutate:  func(b *observe.Block) { b.Facts = map[string]string{"": "item.x"} },
			wantErr: "empty fact name",
		},
		{
			name:    "uncompilable when: rejected",
			mutate:  func(b *observe.Block) { b.When = `result.success &&` }, // syntax error
			wantErr: "CEL compile",
		},
		{
			name:    "uncompilable forEach: rejected",
			mutate:  func(b *observe.Block) { b.ForEach = `result.results.` }, // syntax error
			wantErr: "CEL compile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := observe.Block{
				When:     valid.When,
				ForEach:  valid.ForEach,
				Subjects: append([]observe.SubjectExpr(nil), valid.Subjects...),
				Facts:    map[string]string{},
			}
			for k, v := range valid.Facts {
				b.Facts[k] = v
			}
			tc.mutate(&b)

			err := observe.ValidateBlock(b)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
