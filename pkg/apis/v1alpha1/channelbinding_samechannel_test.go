package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChannelBindingSameChannelAs(t *testing.T) {
	base := &ChannelBinding{Name: "in", External: map[string]string{"channel_id": "C1"}}
	cases := []struct {
		name  string
		other *ChannelBinding
		want  bool
	}{
		{"nil other means same (output defaults to input)", nil, true},
		{"same channel_id", &ChannelBinding{Name: "out", External: map[string]string{"channel_id": "C1"}}, true},
		{"different channel_id", &ChannelBinding{Name: "out", External: map[string]string{"channel_id": "C2"}}, false},
		{"no ids, same CR name", &ChannelBinding{Name: "in"}, true},
		{"no ids, different CR name", &ChannelBinding{Name: "other"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, base.SameChannelAs(tc.other))
		})
	}
}
