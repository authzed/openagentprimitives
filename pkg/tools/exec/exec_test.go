package exec_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// Request must carry no sandbox identity. An Executor is bound to exactly one
// sandbox, so an identity field here would be a second, contradictable source
// of truth for which sandbox a call targets.
func TestRequest_CarriesNoSandboxIdentity(t *testing.T) {
	tr := reflect.TypeOf(exec.Request{})

	for _, banned := range []string{"Namespace", "Pod", "Container", "Handle", "Target"} {
		_, found := tr.FieldByName(banned)
		assert.False(t, found,
			"exec.Request must not carry %q: the executor is already bound to its sandbox", banned)
	}

	assert.Equal(t, 3, tr.NumField(), "Request is Command, Env, Stdin — nothing else")
}
