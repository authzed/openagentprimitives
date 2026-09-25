package agentcmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func runAgentUninstall(t *testing.T, kb *kube.Bundle, name string) (string, error) {
	t.Helper()
	root := NewCmd(graphGlobals(kb))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"uninstall", name})
	err := root.Execute()
	return out.String(), err
}

func TestAgentUninstallGraphDeletesRootAndChild(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{})
	installOut, err := runAgentInstall(t, graphGlobals(kb), dir)
	require.NoErrorf(t, err, "graph fixture install; out=%s", installOut)
	graphAgentClass(t, kb, graphFixtureRoot)
	graphAgentClass(t, kb, "test-coordinator-reviewer")

	out, err := runAgentUninstall(t, kb, graphFixtureRoot)

	require.NoError(t, err)
	assert.Contains(t, out, "2 resources deleted for install test-coordinator")
	for _, name := range []string{graphFixtureRoot, "test-coordinator-reviewer"} {
		got := &unstructured.Unstructured{}
		got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
		got.SetKind("AgentClass")
		getErr := kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: name}, got)
		assert.True(t, apierrors.IsNotFound(getErr), name)
	}
}
