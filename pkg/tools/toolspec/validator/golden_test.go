package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

type goldenInvocation struct {
	Command       string            `json:"command"`
	Argv          []string          `json:"argv"`
	Env           map[string]string `json:"env,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	BinaryVersion string            `json:"binaryVersion,omitempty"`
}

type goldenExpectTrace struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type goldenExpect struct {
	Allow         bool                `json:"allow"`
	FailedOn      *CheckRef           `json:"failedOn,omitempty"`
	TraceContains []goldenExpectTrace `json:"traceContains,omitempty"`
	Redactions    map[string]struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"redactions,omitempty"`
}

type goldenCase struct {
	Name       string           `json:"name"`
	Toolkit    string           `json:"toolkit"` // relative to cases dir's parent
	Spec       string           `json:"spec"`
	Invocation goldenInvocation `json:"invocation"`
	Expect     goldenExpect     `json:"expect"`
}

// TestGolden iterates pkg/tools/toolspec/validator/testdata/<tool>/cases/*.yaml.
func TestGolden(t *testing.T) {
	base := "testdata"
	tools, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		t.Skip("no testdata yet")
	}
	require.NoError(t, err, "read testdata dir")
	for _, tool := range tools {
		if !tool.IsDir() {
			continue
		}
		casesDir := filepath.Join(base, tool.Name(), "cases")
		files, err := os.ReadDir(casesDir)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err, "read cases dir %s", casesDir)
		for _, f := range files {
			if filepath.Ext(f.Name()) != ".yaml" {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", tool.Name(), f.Name()), func(t *testing.T) {
				runGolden(t, filepath.Join(casesDir, f.Name()), filepath.Join(base, tool.Name()))
			})
		}
	}
}

func runGolden(t *testing.T, casePath, toolDir string) {
	t.Helper()
	data, err := os.ReadFile(casePath)
	require.NoError(t, err, "read case %s", casePath)
	var gc goldenCase
	require.NoError(t, yaml.Unmarshal(data, &gc), "unmarshal %s", casePath)
	tk, err := toolkit.Load(filepath.Join(toolDir, gc.Toolkit))
	require.NoError(t, err, "load toolkit %s", gc.Toolkit)
	sp, err := spec.Load(filepath.Join(toolDir, gc.Spec))
	require.NoError(t, err, "load spec %s", gc.Spec)
	d, err := Check(tk, sp, Invocation{
		Command:       gc.Invocation.Command,
		Argv:          gc.Invocation.Argv,
		Env:           gc.Invocation.Env,
		Cwd:           gc.Invocation.Cwd,
		BinaryVersion: gc.Invocation.BinaryVersion,
	})
	require.NoError(t, err, "check")
	assert.Equal(t, gc.Expect.Allow, d.Allow, "Allow (failedOn=%+v)", d.FailedOn)
	if gc.Expect.FailedOn != nil {
		if assert.NotNil(t, d.FailedOn, "FailedOn want %+v", gc.Expect.FailedOn) {
			assert.Equal(t, gc.Expect.FailedOn.Path, d.FailedOn.Path, "FailedOn.Path")
		}
	}
	// traceContains as unordered subset check:
	for _, want := range gc.Expect.TraceContains {
		found := false
		for _, got := range d.Trace {
			if got.Path == want.Path && got.Status == want.Status {
				found = true
				break
			}
		}
		assert.True(t, found, "trace missing %+v (got %+v)", want, d.Trace)
	}
	for id, want := range gc.Expect.Redactions {
		got, ok := d.Redactions[id]
		if !assert.True(t, ok, "expected redaction id=%s", id) {
			continue
		}
		assert.Equal(t, want.Kind, got.Kind, "redaction[%s].Kind", id)
		assert.Equal(t, want.Name, got.Name, "redaction[%s].Name", id)
	}
}
