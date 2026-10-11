package judge_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
)

func TestDeterministicName(t *testing.T) {
	assert.Equal(t, "deterministic", judge.NewDeterministic().Name())
}

func TestDeterministicClassify(t *testing.T) {
	cases := []struct {
		name   string
		action judge.Action
		want   judge.Tier
	}{
		{
			name: "plain link-follow resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "pricing",
				FormAction: "/pricing", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
		{
			name: "search role textbox entry resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "searchbox", Text: "search",
				Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
		{
			name: "generic textbox labeled search resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "search products",
				Host: "example.com", PageURL: "https://example.com/catalog",
			},
			want: judge.TierNavigate,
		},
		{
			name: "tab switch resolves to observe",
			action: judge.Action{
				Op: "click", Role: "tab", Text: "reviews",
				Host: "example.com", PageURL: "https://example.com/product",
			},
			want: judge.TierObserve,
		},
		{
			name: "unknown consequential click on a button fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "button", Text: "buy now",
				FormAction: "/purchase", Host: "example.com", PageURL: "https://example.com/cart",
			},
			want: judge.TierMutate,
		},
		{
			name: "link text carrying a mutating verb fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "delete account",
				FormAction: "/account/delete", Host: "example.com", PageURL: "https://example.com/settings",
			},
			want: judge.TierMutate,
		},
		{
			name: "link with no real href fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "details",
				FormAction: "", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierMutate,
		},
		{
			name: "unidentified textbox entry fails closed to mutate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "amount",
				Host: "example.com", PageURL: "https://example.com/transfer",
			},
			want: judge.TierMutate,
		},
		{
			name:   "ambiguous empty action fails closed to mutate",
			action: judge.Action{},
			want:   judge.TierMutate,
		},
		{
			name: "destructive href disguised with benign link text fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "Yes, I'm sure",
				FormAction: "/account/delete", Host: "example.com", PageURL: "https://example.com/settings",
			},
			want: judge.TierMutate,
		},
		{
			name: "empty-text link with any href fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "",
				FormAction: "/foo", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierMutate,
		},
		{
			name: "genuine navigation link with benign path and text resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "My trips",
				FormAction: "/trips", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
		{
			name: "destructive href disguised as a tab click fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "tab", Text: "reviews",
				FormAction: "/account/delete", Host: "example.com", PageURL: "https://example.com/product",
			},
			want: judge.TierMutate,
		},
		{
			name: "searchbox role nested in a destructive form fails closed to mutate",
			action: judge.Action{
				Op: "type", Role: "searchbox", Text: "50000",
				FormAction: "/accounts/transfer/execute", Host: "example.com", PageURL: "https://example.com/transfer",
			},
			want: judge.TierMutate,
		},
		{
			name: "search-labeled textbox nested in a destructive form fails closed to mutate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "search",
				FormAction: "/accounts/transfer/execute", Host: "example.com", PageURL: "https://example.com/transfer",
			},
			want: judge.TierMutate,
		},
		{
			name: "genuine search box with a benign form action resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "searchbox", Text: "shoes",
				FormAction: "/search", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
	}

	d := judge.NewDeterministic()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := d.Classify(context.Background(), tc.action, judge.PageContext{})
			require.NoError(t, err, "the deterministic judge must never error")
			assert.Equal(t, tc.want, v.Tier)
			require.NotNil(t, v.Probs, "Probs must always be populated")
			assert.InDelta(t, 1.0, v.Probs[tc.want], 0.0001, "the chosen tier must carry full confidence")
			assert.NotEmpty(t, v.Rationale, "a judge must explain its verdict")

			if tc.want == judge.TierMutate {
				assert.Equal(t, map[judge.Tier]float64{judge.TierMutate: 1.0}, v.Probs,
					"the fail-closed default must be a one-hot mutate distribution")
			}
		})
	}
}
