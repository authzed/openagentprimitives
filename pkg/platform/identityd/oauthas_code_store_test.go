package identityd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthCodeStoreSingleUseAndTTL(t *testing.T) {
	st := newAuthCodeStore(time.Minute)
	code, err := st.NewCode(authCodeEntry{ClientID: "c", Challenge: "ch"})
	require.NoError(t, err)

	got, ok := st.Consume(code)
	require.True(t, ok)
	assert.Equal(t, "ch", got.Challenge)

	_, ok = st.Consume(code)
	assert.False(t, ok, "codes are single-use")

	st2 := newAuthCodeStore(-time.Second) // already expired
	code2, err := st2.NewCode(authCodeEntry{ClientID: "c"})
	require.NoError(t, err)
	_, ok = st2.Consume(code2)
	assert.False(t, ok, "expired codes are refused")
}

func TestPendingAuthStoreSingleUseAndTTL(t *testing.T) {
	st := newPendingAuthStore(time.Minute)
	id, err := st.NewPending(pendingAuthEntry{ClientID: "c", ClientName: "demo tool"})
	require.NoError(t, err)

	got, ok := st.Consume(id)
	require.True(t, ok)
	assert.Equal(t, "demo tool", got.ClientName)

	_, ok = st.Consume(id)
	assert.False(t, ok, "pending entries are single-use")

	st2 := newPendingAuthStore(-time.Second) // already expired
	id2, err := st2.NewPending(pendingAuthEntry{ClientID: "c"})
	require.NoError(t, err)
	_, ok = st2.Consume(id2)
	assert.False(t, ok, "expired pending entries are refused")
}
