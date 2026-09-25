package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTailBuffer_KeepsLastMaxBytes(t *testing.T) {
	cases := []struct {
		name   string
		max    int
		writes [][]byte
		want   []byte
	}{
		{name: "under cap: keeps all", max: 8, writes: [][]byte{[]byte("abc")}, want: []byte("abc")},
		{name: "exactly at cap: keeps all", max: 4, writes: [][]byte{[]byte("abcd")}, want: []byte("abcd")},
		{name: "one over cap in one write: keeps last max", max: 4, writes: [][]byte{[]byte("abcde")}, want: []byte("bcde")},
		{name: "two chunks jointly exceed cap: keeps last max across writes", max: 4, writes: [][]byte{[]byte("abc"), []byte("def")}, want: []byte("cdef")},
		{name: "zero cap: no-op", max: 0, writes: [][]byte{[]byte("abc")}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := &tailBuffer{max: tc.max}
			for _, w := range tc.writes {
				tb.append(w)
			}
			assert.Equal(t, tc.want, tb.Bytes())
		})
	}
}
