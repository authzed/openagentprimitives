package bronzethread

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every bundle written before attachments existed uses the bare-string form,
// and none of them should have to change: migrating ~40 fixtures to add an
// optional field would be all risk and no benefit. So UserTurn must accept
// both shapes.
func TestUserTurnAcceptsBareStringAndObject(t *testing.T) {
	var got []UserTurn
	require.NoError(t, json.Unmarshal([]byte(`[
		"just text",
		{"text": "with a file", "attachments": [
			{"filename": "bundle.zip", "mime": "application/zip", "file": "files/bundle.zip"}
		]}
	]`), &got))

	require.Len(t, got, 2)
	assert.Equal(t, "just text", got[0].Text)
	assert.Empty(t, got[0].Attachments, "a bare string carries no attachments")
	assert.Equal(t, "with a file", got[1].Text)
	require.Len(t, got[1].Attachments, 1)
	assert.Equal(t, "bundle.zip", got[1].Attachments[0].Filename)
	assert.Equal(t, "application/zip", got[1].Attachments[0].MIME)
	assert.Equal(t, "files/bundle.zip", got[1].Attachments[0].File)
}

func TestUserTurnRejectsIncompleteAttachment(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no file: nothing to serve bytes from",
			in:   `[{"text":"x","attachments":[{"filename":"a.txt","mime":"text/plain"}]}]`,
			want: "file",
		},
		{
			name: "no filename: the turn cannot name what it attached",
			in:   `[{"text":"x","attachments":[{"file":"files/a.txt","mime":"text/plain"}]}]`,
			want: "filename",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []UserTurn
			err := json.Unmarshal([]byte(tc.in), &got)
			require.Error(t, err, "an incomplete attachment must fail the load, not arrive half-built")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// An attachment path is read relative to the bundle directory and must not
// escape it: a fixture is not a place to exercise the host filesystem.
func TestUserTurnRejectsEscapingFilePath(t *testing.T) {
	for _, p := range []string{"../outside.txt", "/etc/passwd"} {
		var got []UserTurn
		err := json.Unmarshal([]byte(`[{"text":"x","attachments":[{"filename":"a.txt","file":"`+p+`"}]}]`), &got)
		require.Error(t, err, "file %q must be rejected", p)
	}
}

// The whole point of the change: a Bundle still round-trips the old shape.
func TestBundleStillLoadsBareStringTurns(t *testing.T) {
	var b Bundle
	require.NoError(t, json.Unmarshal([]byte(`{"name":"x","userTurns":["hello","world"]}`), &b))
	require.Len(t, b.UserTurns, 2)
	assert.Equal(t, "hello", b.UserTurns[0].Text)
	assert.Equal(t, "world", b.UserTurns[1].Text)
}
