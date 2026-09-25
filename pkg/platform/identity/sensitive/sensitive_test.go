package sensitive

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSensitiveValue_RedactsEverywhereButUnderlyingValue(t *testing.T) {
	s := NewSensitiveValue([]byte("super-secret-token"))
	assert.Equal(t, []byte("super-secret-token"), s.UnderlyingValue(), "UnderlyingValue exposes bytes")
	assert.False(t, s.IsEmpty())
	assert.Equal(t, "[REDACTED]", s.String(), "String redacts")
	assert.Equal(t, "[REDACTED]", fmt.Sprintf("%s", s), "%s redacts")
	assert.Equal(t, "[REDACTED]", fmt.Sprintf("%v", s), "%v redacts")
	assert.Equal(t, "[REDACTED]", fmt.Sprintf("%#v", s), "%#v redacts (GoString)")
	b, err := json.Marshal(map[string]SensitiveValue{"tok": s})
	assert.NoError(t, err)
	assert.JSONEq(t, `{"tok":"[REDACTED]"}`, string(b), "JSON redacts")

	assert.True(t, NewSensitiveValue(nil).IsEmpty())
	assert.True(t, NewSensitiveValue([]byte{}).IsEmpty())
}
