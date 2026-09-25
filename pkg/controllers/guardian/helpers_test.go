// pkg/controllers/guardian/helpers_test.go
//
// Shared test helpers for the guardian package.  No build tag: available
// to both the unit build and the integration build so that untagged files
// (e.g. spicedbbootstrap_controller_test.go) and integration-tagged files
// (e.g. agentsessiongrants_controller_test.go) can share these types.
package guardian_test

import (
	"context"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeSchemaIO is the test stand-in for guardianschema.SchemaIO. It
// records every ReadSchema/WriteSchema pair and supports injecting
// errors.
type fakeSchemaIO struct {
	mu sync.Mutex

	cur      string
	readErr  error
	writeErr error

	reads  int
	writes []string
}

func (f *fakeSchemaIO) ReadSchema(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.cur, nil
}

func (f *fakeSchemaIO) WriteSchema(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, text)
	f.cur = text
	return nil
}

func (f *fakeSchemaIO) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// lastWrite returns the most recent text passed to WriteSchema, or ""
// if none. Used to assert against the composed schema text in
// integration-style controller tests.
func (f *fakeSchemaIO) lastWrite() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		return ""
	}
	return f.writes[len(f.writes)-1]
}

func findCondition(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}
