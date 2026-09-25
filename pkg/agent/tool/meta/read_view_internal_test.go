package meta

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// jsonTagsOf returns the wire name of every field of a struct type, in
// declaration order, with any ",omitempty"/",string" option stripped. A field
// tagged `json:"-"` is skipped (it can never reach the wire); an untagged
// field contributes its Go name, because that is what encoding/json emits.
func jsonTagsOf(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

// TestReadViewResultCarriesNoOtherFieldByType pins read_view's payload
// STRUCTURALLY — over the type, not over one execution's marshalled output.
//
// The distinction is the whole finding this test answers. Its sibling in
// read_view_test.go asserts the key set of a REAL Execute's JSON, which can
// only see a field that was non-empty on that run: a fifth field tagged
// `omitempty` is invisible to it whenever the fixture happens to have nothing
// behind the binding that would fill it. (The fixture now plants real content
// behind every memory-reachable binding source precisely so that assertion is
// not hostage to an empty scope — see plantBindableData — but "the fixture has
// data today" is a property of the fixture, and this is a property of the
// code.) Reading the tags off readViewResult itself is blind to content: a new
// field fails here whether or not anything ever populates it.
//
// The design's central prohibition is that UI data reaches the model through
// exactly ONE metered path — the agent calling the binding's own tool. A
// values field of any name, from any source, empty or not, is a second one.
func TestReadViewResultCarriesNoOtherFieldByType(t *testing.T) {
	want := []string{"declaration", "agentComposed", "parameters", "rejected", "jsx"}
	got := jsonTagsOf(reflect.TypeOf(readViewResult{}))

	assert.ElementsMatch(t, want, got,
		"read_view's payload type must declare ONLY the declaration, the composed-slot marker, current "+
			"parameters, rejections, and the JSX printout — never a data field of any kind, and `omitempty` is no exemption")
}
