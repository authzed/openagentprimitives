package html

import "testing"

// TestIsArtifactRefValue exercises the unexported isArtifactRefValue against
// well-formed handles/tags and adversarial values — notably the path-traversal
// ".." variants a naive charset+prefix regex would let through. Cases pass the
// already-reassembled opaque value (handle, or handle#tag) in the same shape
// the production callback in New() builds from u.Opaque + "#" + u.Fragment.
func TestIsArtifactRefValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// --- accept: well-formed handles, with and without a tag ---
		{name: "ar- handle", in: "ar-abc123", want: true},
		{name: "artifact- handle", in: "artifact-3f2a1b8c", want: true},
		{name: "artrev- handle", in: "artrev-9d4e0117", want: true},
		{name: "ar- handle with tag", in: "ar-abc#draft", want: true},
		{name: "artifact- handle with dotted tag", in: "artifact-x#v1.0", want: true},

		// --- reject: empty / malformed ---
		{name: "empty string", in: "", want: false},

		// --- reject: path traversal ---
		{name: "bare ..", in: "..", want: false},
		{name: "ar- handle is ..", in: "ar-..", want: false},
		{name: "artifact- handle is ..", in: "artifact-..", want: false},
		{name: "tag is ..", in: "ar-x#..", want: false},
		{name: "traversal path in handle", in: "ar-x/../../y", want: false},
		{name: "traversal-heavy value", in: "../../../etc/passwd", want: false},

		// --- reject: disallowed characters / shape ---
		{name: "query string in handle", in: "ar-x?a=b", want: false},
		{name: "double fragment", in: "ar-x#a#b", want: false},
		{name: "leading space", in: " ar-x", want: false},
		{name: "host/path shape (artifact://evil.com/x)", in: "evil.com/x", want: false},
		{name: "javascript: scheme value", in: "javascript:alert(1)", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isArtifactRefValue(tc.in)
			if got != tc.want {
				t.Errorf("isArtifactRefValue(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
