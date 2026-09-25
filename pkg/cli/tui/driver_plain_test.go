package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedScreen asks one question and records the answer under its own key.
// Three of them in a row is the minimum that exercises reader reuse ACROSS
// presentations, which is where the plain driver's input handling is load
// bearing.
type scriptedScreen struct {
	key string
	val string
}

func (s *scriptedScreen) ID() string    { return s.key }
func (s *scriptedScreen) Label() string { return s.key }

func (s *scriptedScreen) Prepare(context.Context, *State) (*huh.Group, error) {
	return huh.NewGroup(huh.NewInput().Key(s.key).Title(s.key).Value(&s.val)), nil
}

func (s *scriptedScreen) Apply(_ context.Context, st *State) error {
	st.Set(s.key, s.val)
	return nil
}

// TestPlainDriverGivesEachScreenItsOwnScriptedLine is a regression test for
// silent answer loss. huh builds a fresh, greedily-filling bufio.Scanner per
// field, so an unbuffered multi-line reader is consumed entirely by the first
// screen; screens two and three then read EOF, take their defaults, and the
// run reports success with blank answers. Asserting the LAST screen's answer
// is what discriminates: a driver with this bug passes any test that only
// checks the first.
func TestPlainDriverGivesEachScreenItsOwnScriptedLine(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("first\nsecond\nthird\n")
	screens := []Screen{
		&scriptedScreen{key: "one"},
		&scriptedScreen{key: "two"},
		&scriptedScreen{key: "three"},
	}

	st, err := Run(context.Background(), screens,
		Options{Theme: NewTheme(Caps{}), In: in, Out: &out})

	require.NoError(t, err, "the plain driver must drive every screen")
	assert.Equal(t, "first", st.Get("one"))
	assert.Equal(t, "second", st.Get("two"), "the second screen must not read past its own line")
	assert.Equal(t, "third", st.Get("three"), "the third screen must still have input left to read")
}

// TestPlainDriverWithNoInputIsLeftForHuhToDefault pins that a nil reader stays
// nil: huh reads os.Stdin when handed no input, and wrapping nil would give it
// a non-nil reader with nothing behind it to read.
func TestPlainDriverWithNoInputIsLeftForHuhToDefault(t *testing.T) {
	d := Plain(nil, io.Discard, nil)

	require.IsType(t, &plainDriver{}, d)
	assert.Nil(t, d.(*plainDriver).in, "a nil reader must not be wrapped")
}

func TestLineReaderNeverReadsPastANewline(t *testing.T) {
	cases := []struct {
		name  string
		input string
		bufSz int
		want  []string
	}{
		{
			name:  "multi-line input: one line per Read, newline retained",
			input: "a\nbb\nccc\n",
			bufSz: 64,
			want:  []string{"a\n", "bb\n", "ccc\n"},
		},
		{
			name:  "final line without a trailing newline: returned as its own read",
			input: "a\nb",
			bufSz: 64,
			want:  []string{"a\n", "b"},
		},
		{
			name:  "buffer smaller than the line: split at the buffer, not lost",
			input: "abcdef\n",
			bufSz: 4,
			want:  []string{"abcd", "ef\n"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLineReader(strings.NewReader(tc.input))
			var got []string
			for {
				buf := make([]byte, tc.bufSz)
				n, err := r.Read(buf)
				if n > 0 {
					got = append(got, string(buf[:n]))
				}
				if err != nil {
					assert.ErrorIs(t, err, io.EOF, "the only expected terminal error is EOF")
					break
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// onceErrReader yields data and then reports err exactly once, the way a
// network-backed reader reports a transport failure. A second Read reports
// EOF, so an error that is not surfaced the first time is gone for good.
type onceErrReader struct {
	data []byte
	err  error
	done bool
}

func (r *onceErrReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, r.err
}

// TestLineReaderSurfacesAnErrorDeferredBehindData: when a read has both bytes
// and an error, lineReader returns the bytes first and the error on the next
// call. Dropping it would be a silently swallowed error — and bufio.Reader
// will not re-report it, because it clears its stored error once read.
func TestLineReaderSurfacesAnErrorDeferredBehindData(t *testing.T) {
	boom := errors.New("transport failed")
	r := newLineReader(&onceErrReader{data: []byte("partial"), err: boom})

	buf := make([]byte, 64)
	n, err := r.Read(buf)
	require.NoError(t, err, "the buffered bytes come back before the error")
	assert.Equal(t, "partial", string(buf[:n]))

	_, err = r.Read(buf)
	assert.ErrorIs(t, err, boom, "the deferred error must be reported, not dropped")
}

func TestLineReaderIntoAZeroLengthBufferReadsNothing(t *testing.T) {
	r := newLineReader(strings.NewReader("a\n"))
	n, err := r.Read(nil)
	require.NoError(t, err, "a zero-length read must not consume input or report EOF")
	assert.Zero(t, n)
}
