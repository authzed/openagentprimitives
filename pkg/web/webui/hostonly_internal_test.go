package webui

import "testing"

func TestHostOnly(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "full https URL → host", in: "https://x.ngrok-free.app", want: "x.ngrok-free.app"},
		{name: "full https URL with port → host", in: "https://x.example:8443", want: "x.example"},
		{name: "full https URL with path → host", in: "https://x.example/artifact-view?d=1", want: "x.example"},
		{name: "bare host → itself", in: "trusted.example", want: "trusted.example"},
		{name: "host:port → host", in: "trusted.example:8080", want: "trusted.example"},
		{name: "empty → empty", in: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostOnly(tc.in); got != tc.want {
				t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
