package toolcall

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

func TestExecutorFor(t *testing.T) {
	cases := []struct {
		name    string
		handle  *spiceboxv1alpha1.SandboxHandle
		wantErr string
	}{
		{
			name:    "no handle yet: refused, not guessed",
			handle:  nil,
			wantErr: "no sandbox",
		},
		{
			name:    "kind with no runtime: refused, naming the kind",
			handle:  &spiceboxv1alpha1.SandboxHandle{Kind: "never-registered", Ref: "x"},
			wantErr: "never-registered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{Runtimes: sandboxkinds.Runtimes{}}
			sess := &spiceboxv1alpha1.SpiceboxSession{}
			sess.Status.Sandbox = tc.handle

			_, err := r.executorFor(sess)
			require.Error(t, err, "a tool call must never run in a guessed sandbox")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
