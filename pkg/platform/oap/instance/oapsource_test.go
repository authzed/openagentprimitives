package instance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshalParseOapSource_RoundTrip covers a registry-sourced install (Ref
// populated) and a file-sourced install (Ref deliberately empty, per
// OapSource's doc comment) — both must survive Marshal -> Parse unchanged.
func TestMarshalParseOapSource_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		src  OapSource
	}{
		{
			name: "registry source: Ref/Digest/Version/SourceKind all preserved",
			src: OapSource{
				Ref:        "ghcr.io/example/demo-agent:1",
				Digest:     "sha256:abcd1234",
				Version:    "1.2.0",
				SourceKind: "registry",
			},
		},
		{
			name: "file source: empty Ref preserved as empty, rest unchanged",
			src: OapSource{
				Ref:        "",
				Digest:     "sha256:deadbeef",
				Version:    "0.1.0",
				SourceKind: "file",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := MarshalOapSource(tc.src)
			require.NoError(t, err, "MarshalOapSource")

			got, err := ParseOapSource(raw)
			require.NoError(t, err, "ParseOapSource")

			assert.Equal(t, tc.src.Ref, got.Ref, "Ref")
			assert.Equal(t, tc.src.Digest, got.Digest, "Digest")
			assert.Equal(t, tc.src.Version, got.Version, "Version")
			assert.Equal(t, tc.src.SourceKind, got.SourceKind, "SourceKind")
		})
	}
}

// TestMarshalOapSource_Deterministic pins the SSA-idempotency invariant at the
// annotation-value layer: marshaling the SAME OapSource twice must produce the
// exact same bytes. If a timestamp (or any other non-bundle-derived, changing
// field) ever creeps back into OapSource, a re-install would write a different
// annotation value and stop being an SSA no-op — this catches that.
func TestMarshalOapSource_Deterministic(t *testing.T) {
	src := OapSource{
		Ref:        "ghcr.io/example/demo-agent:1",
		Digest:     "sha256:abcd1234",
		Version:    "1.2.0",
		SourceKind: "registry",
	}
	a, err := MarshalOapSource(src)
	require.NoError(t, err, "MarshalOapSource #1")
	b, err := MarshalOapSource(src)
	require.NoError(t, err, "MarshalOapSource #2")
	assert.Equal(t, a, b, "the same OapSource must marshal to byte-identical annotation content")
}

// TestParseOapSource_InvalidJSON confirms ParseOapSource fails closed on
// malformed annotation content rather than returning a zero-value OapSource
// that a caller could mistake for "no source recorded".
func TestParseOapSource_InvalidJSON(t *testing.T) {
	_, err := ParseOapSource("not json")
	assert.Error(t, err, "ParseOapSource must reject malformed JSON")
}
