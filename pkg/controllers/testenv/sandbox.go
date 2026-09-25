package testenv

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	execfake "github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// SandboxRuntimes builds one Runtime per sandbox kind registered in the
// process (via each kind's own blank import, e.g. `_
// ".../sandboxkinds/pod"`) against c, execing through a throwaway fake
// binder. Every spiceboxsession.Reconciler an integration test constructs
// needs this: without a populated Runtimes, a session's resolved kind never
// resolves — Runtimes.For is fail-closed, not a fallback to the built-in
// backend — so no sandbox is ever created and any wait for one times out.
//
// A test whose controller under test also execs into the sandbox (the
// ToolCall controller) must use SandboxRuntimesWithExec instead — the
// throwaway binder here is never programmed, so its responses would never be
// found.
func SandboxRuntimes(t *testing.T, c client.Client) sandboxkinds.Runtimes {
	t.Helper()
	return SandboxRuntimesWithExec(t, c, execfake.New().For)
}

// SandboxRuntimesWithExec is SandboxRuntimes, but binds every kind's exec
// transport to execFor instead of an internal throwaway fake. Pass a
// fake.Binder's For method to let the ToolCall controller's execs land on
// the same Binder the test programs and asserts against.
func SandboxRuntimesWithExec(t *testing.T, c client.Client, execFor func(namespace, pod, container string) exec.Executor) sandboxkinds.Runtimes {
	t.Helper()
	rts := sandboxkinds.Runtimes{}
	for _, k := range sandboxregistry.All() {
		// Mirror the operator's own log-and-skip (internal/cmd/operator/main.go): the
		// sandbox kind registry is process-global within a test binary, so a
		// kind another test file registers specifically to be unconstructible
		// (e.g. to prove a validation path rejects a class its backend
		// refuses) must not hard-fail every other test in the package that
		// builds runtimes. A kind that cannot be constructed in this
		// environment just has no runtime here, same as a bring-your-own
		// backend whose peer CRD isn't installed.
		rt, err := k.NewRuntime(sandboxkinds.Deps{Client: c, ExecFor: execFor})
		if err != nil {
			t.Logf("sandbox kind %q unavailable; skipping runtime construction: %s", k.Name(), err.Error())
			continue
		}
		rts[k.Name()] = rt
	}
	return rts
}
