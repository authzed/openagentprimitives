package registry_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/registry"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// stubModality is a minimal modality.Modality for registry tests.
type stubModality struct {
	name string
}

func (s *stubModality) Name() string                           { return s.name }
func (s *stubModality) MetaTools(env modality.Env) []tool.Tool { return nil }
func (s *stubModality) Instructions(env modality.Env) string   { return "" }

var _ modality.Modality = (*stubModality)(nil)

func TestRegisterAndAllSorted(t *testing.T) {
	snap := registry.SnapshotForTest()
	registry.RestoreForTest(map[string]modality.Modality{})
	t.Cleanup(func() { registry.RestoreForTest(snap) })

	registry.Register(&stubModality{name: "zzz"})
	registry.Register(&stubModality{name: "aaa"})

	all := registry.All()
	require.Len(t, all, 2)
	require.Equal(t, "aaa", all[0].Name())
	require.Equal(t, "zzz", all[1].Name())

	require.Panics(t, func() {
		registry.Register(&stubModality{name: "aaa"})
	})
}
