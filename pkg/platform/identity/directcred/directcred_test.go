package directcred_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
)

// fakeSource is a minimal Source impl for registry tests. It records
// nothing and returns zero values — the registry tests exercise
// Register/Get/All wiring, not any backend behavior.
type fakeSource struct{ kind string }

func (f fakeSource) Kind() string { return f.kind }

func (f fakeSource) Verify(ctx context.Context, cfg directcred.Config) (directcred.Pin, error) {
	return directcred.Pin{}, nil
}

func (f fakeSource) List(ctx context.Context, cfg directcred.Config) ([]directcred.Ref, error) {
	return nil, nil
}

func (f fakeSource) Fetch(ctx context.Context, cfg directcred.Config, id string) (directcred.Credential, error) {
	return directcred.Credential{}, nil
}

func TestRegisterGetAll(t *testing.T) {
	directcred.Reset()
	t.Cleanup(directcred.Reset)

	// Register out of order; All must come back sorted by Kind().
	directcred.Register(fakeSource{kind: "secret"})
	directcred.Register(fakeSource{kind: "onepassword"})

	got, ok := directcred.Get("onepassword")
	require.True(t, ok, "Get(onepassword) must hit")
	assert.Equal(t, "onepassword", got.Kind())

	all := directcred.All()
	require.Len(t, all, 2)
	assert.Equal(t, []string{"onepassword", "secret"},
		[]string{all[0].Kind(), all[1].Kind()}, "All must be sorted by Kind()")
}

func TestGetUnknownReturnsFalse(t *testing.T) {
	directcred.Reset()
	t.Cleanup(directcred.Reset)

	directcred.Register(fakeSource{kind: "secret"})

	_, ok := directcred.Get("nope")
	assert.False(t, ok, "Get(nope) should miss")
}

func TestRegisterDuplicatePanics(t *testing.T) {
	directcred.Reset()
	t.Cleanup(directcred.Reset)

	directcred.Register(fakeSource{kind: "secret"})
	assert.Panics(t, func() {
		directcred.Register(fakeSource{kind: "secret"})
	}, "duplicate Kind() must panic")
}

// TestConstantsMatchKind locks SourceOnePassword/SourceSecret to the kind
// strings later tasks (the two backends) register under.
func TestConstantsMatchKind(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"SourceOnePassword", directcred.SourceOnePassword, "onepassword"},
		{"SourceSecret", directcred.SourceSecret, "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}
}
