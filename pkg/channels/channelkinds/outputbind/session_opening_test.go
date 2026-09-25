package outputbind_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// stubKind answers only the three questions SessionOpening's dispatch asks: the
// registry name, whether this kind's inbound carries a human, and — on the
// describing variant below — the trigger sentence. Every other Kind method is
// inherited from the embedded nil interface, so a lookup this test never meant
// to make panics loudly instead of quietly answering.
type stubKind struct {
	channelkinds.Kind
	name         string
	attributable bool
}

func (s stubKind) Name() string           { return s.name }
func (s stubKind) UserAttributable() bool { return s.attributable }

// describingStubKind is stubKind that also implements TriggerDescriber, so the
// human/no-human gate can be tested in isolation: both the userless and the
// attributable row below can describe their trigger, and only the gate differs.
type describingStubKind struct {
	stubKind
	text string
	ok   bool
}

func (d describingStubKind) DescribeTrigger(_ *spiceboxv1alpha1.Channel, _, _ string, _ []byte) (string, bool) {
	return d.text, d.ok
}

// registerStubKinds installs the fixtures for this file and restores the
// process-wide registry (the real kinds this package's other tests blank-import
// at init, which never runs twice) on cleanup. Callers must stay serial.
func registerStubKinds(t *testing.T, kinds ...channelkinds.Kind) {
	t.Helper()
	prior := registry.All()
	t.Cleanup(func() {
		registry.Reset()
		for _, k := range prior {
			registry.Register(k)
		}
	})
	for _, k := range kinds {
		registry.Register(k)
	}
}

func chOf(kind, role string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-input", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: kind, Role: role},
	}
}

// TestSessionOpening pins which sessions get a synthesized opening line, and
// which are left exactly as they are today.
//
// The row that matters most is the attributable one: it describes its trigger
// just as well as the userless row does, so the ONLY thing standing between a
// human's own message and a synthesized line posted in front of it is the
// no-human gate.
func TestSessionOpening(t *testing.T) {
	registerStubKinds(t,
		describingStubKind{stubKind{name: "stub-userless"}, "Picked up pull request demo-org/demo-repo#2.", true},
		describingStubKind{stubKind{name: "stub-human", attributable: true}, "Picked up pull request demo-org/demo-repo#2.", true},
		describingStubKind{stubKind{name: "stub-declines"}, "", false},
		describingStubKind{stubKind{name: "stub-blank"}, "   \n ", true},
		stubKind{name: "stub-silent"},
	)

	cases := []struct {
		name     string
		ch       *spiceboxv1alpha1.Channel
		wantText string
		wantOK   bool
	}{
		{
			name:     "userless input that describes its trigger: its line opens the thread",
			ch:       chOf("stub-userless", spiceboxv1alpha1.ChannelRoleInput),
			wantText: "Picked up pull request demo-org/demo-repo#2.",
			wantOK:   true,
		},
		{
			name:   "human-initiated input, same describer: nothing synthesized (their message is the root)",
			ch:     chOf("stub-human", spiceboxv1alpha1.ChannelRoleInput),
			wantOK: false,
		},
		{
			name:   "userless input whose kind cannot describe itself: nothing (today's behaviour)",
			ch:     chOf("stub-silent", spiceboxv1alpha1.ChannelRoleInput),
			wantOK: false,
		},
		{
			name:   "userless input whose kind declines this key: nothing",
			ch:     chOf("stub-declines", spiceboxv1alpha1.ChannelRoleInput),
			wantOK: false,
		},
		{
			name:   "userless input that renders blank: nothing (an empty opening is worse than none)",
			ch:     chOf("stub-blank", spiceboxv1alpha1.ChannelRoleInput),
			wantOK: false,
		},
		{
			name:   "role=output: it delivers no inbound, so there is no trigger to describe",
			ch:     chOf("stub-userless", spiceboxv1alpha1.ChannelRoleOutput),
			wantOK: false,
		},
		{
			name:   "unregistered kind: nothing (the caller's own path refuses it by name)",
			ch:     chOf("nosuch", spiceboxv1alpha1.ChannelRoleInput),
			wantOK: false,
		},
		{
			name:   "nil Channel: nothing",
			ch:     nil,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, ok := outputbind.SessionOpening(tc.ch, "pr:demo-org/demo-repo#2", "", nil)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantText, text)
		})
	}
}
