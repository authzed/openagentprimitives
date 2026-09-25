package memory_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestRegisterKind_RoundTrip(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	k := fakeKind{name: "alpha", prefix: "a-"}
	memory.RegisterKind(k)
	got, ok := memory.LookupKind("alpha")
	require.True(t, ok)
	assert.Equal(t, "alpha", got.Name())
	assert.Equal(t, "a-", got.IDPrefix())
}

func TestRegisterKind_RejectsEmpty(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	cases := []struct {
		name string
		k    fakeKind
	}{
		{"empty name panics", fakeKind{name: "", prefix: "x-"}},
		{"empty prefix panics", fakeKind{name: "x", prefix: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Panics(t, func() { memory.RegisterKind(tc.k) })
		})
	}
}

func TestRegisterKind_RejectsUnderscorePrefix(t *testing.T) {
	memory.ResetRegistryForTest()
	t.Cleanup(memory.ResetRegistryForTest)
	assert.PanicsWithValue(t,
		`memory.RegisterKind: Kind name "_query" must not start with "_" (reserved for HTTP scope routes)`,
		func() { memory.RegisterKind(fakeKind{name: "_query", prefix: "q-"}) },
	)
}

func TestRegisterKind_CollisionsPanic(t *testing.T) {
	cases := []struct {
		name   string
		first  fakeKind
		second fakeKind
	}{
		{
			"duplicate name panics",
			fakeKind{name: "alpha", prefix: "a-"},
			fakeKind{name: "alpha", prefix: "b-"},
		},
		{
			"duplicate prefix panics",
			fakeKind{name: "alpha", prefix: "a-"},
			fakeKind{name: "beta", prefix: "a-"},
		},
		{
			"prefix-of-another panics (op- then op-long-)",
			fakeKind{name: "alpha", prefix: "op-"},
			fakeKind{name: "beta", prefix: "op-long-"},
		},
		{
			"another-prefix-of panics (op-long- then op-)",
			fakeKind{name: "alpha", prefix: "op-long-"},
			fakeKind{name: "beta", prefix: "op-"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(memory.ResetRegistryForTest)
			memory.RegisterKind(tc.first)
			assert.Panics(t, func() { memory.RegisterKind(tc.second) })
		})
	}
}
