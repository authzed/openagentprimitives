package subjectresolve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeResolver is a minimal Resolver for exercising the registry in
// isolation from the three built-ins.
type fakeResolver struct {
	form, desc string
}

func (f fakeResolver) Usage() (string, string) { return f.form, f.desc }

func (f fakeResolver) TryResolve(context.Context, string, Env) (Resolution, bool, error) {
	return Resolution{}, false, nil
}

func TestUsages_BuiltinsInOrder_FallbackLast(t *testing.T) {
	got := Usages()
	require.Len(t, got, 3, "email, trigger-author, and the generic resource fallback")
	for i, u := range got {
		assert.NotEmpty(t, u.Form, "entry %d: Form must not be empty", i)
		assert.NotEmpty(t, u.Description, "entry %d: Description must not be empty", i)
	}
	assert.Equal(t, "trigger-author", got[1].Form, "trigger-author is a specific scheme, not the fallback")
	last := got[len(got)-1]
	assert.Equal(t, "<type>:<id>", last.Form, "the generic resource fallback must be listed last")
}

func TestRegister_EmptyFormPanics(t *testing.T) {
	assert.Panics(t, func() {
		Register(fakeResolver{form: "", desc: "something"})
	}, "a resolver with an empty form must not be registrable")
}

func TestRegister_EmptyDescriptionPanics(t *testing.T) {
	assert.Panics(t, func() {
		Register(fakeResolver{form: "made-up-scheme", desc: ""})
	}, "a resolver with an empty description must not be registrable")
}

func TestRegister_DuplicateFormPanics(t *testing.T) {
	assert.Panics(t, func() {
		Register(fakeResolver{form: "trigger-author", desc: "a duplicate of the built-in scheme"})
	}, "registering an already-used form must panic")
}
