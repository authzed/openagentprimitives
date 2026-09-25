package subjectresolve

import (
	"context"
	"errors"
)

// fakeRelations is a test double for RelationReader. calls records every
// invocation as "type:id:relation", in order, so tests can assert a
// short-circuit (e.g. "user" never queried once "sole_user" is ambiguous).
type fakeRelations struct {
	subjects map[string][]string
	errs     map[string]error
	calls    []string
}

func (f *fakeRelations) key(objType, objID, relation string) string {
	return objType + ":" + objID + ":" + relation
}

func (f *fakeRelations) UserSubjects(_ context.Context, objType, objID, relation string) ([]string, error) {
	k := f.key(objType, objID, relation)
	f.calls = append(f.calls, k)
	if f.errs != nil {
		if err, ok := f.errs[k]; ok {
			return nil, err
		}
	}
	return f.subjects[k], nil
}

// fakeAnnotations builds an Env.SessionAnnotations func from a fixed map, or
// one that errors.
func fakeAnnotations(m map[string]string) func(ctx context.Context) (map[string]string, error) {
	return func(ctx context.Context) (map[string]string, error) { return m, nil }
}

func fakeAnnotationsErr(err error) func(ctx context.Context) (map[string]string, error) {
	return func(ctx context.Context) (map[string]string, error) { return nil, err }
}

var errFakeAnnotations = errors.New("fake: annotations lookup failed")
var errFakeRelations = errors.New("fake: relations lookup failed")
