package oci

import "testing"

func TestPinnedRef(t *testing.T) {
	const digA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const digB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	cases := []struct {
		name    string
		ref     string
		dig     string
		want    string
		wantErr bool
	}{
		{
			name: "tag replaced by digest",
			ref:  "registry.example/team/demo-agent:v1",
			dig:  digA,
			want: "registry.example/team/demo-agent@" + digA,
		},
		{
			name: "no tag gets digest appended",
			ref:  "registry.example/team/demo-agent",
			dig:  digA,
			want: "registry.example/team/demo-agent@" + digA,
		},
		{
			name: "port in host preserved",
			ref:  "localhost:5000/team/demo-agent:v1",
			dig:  digA,
			want: "localhost:5000/team/demo-agent@" + digA,
		},
		{
			name: "already digest-pinned: replaced with the verified digest",
			ref:  "registry.example/team/demo-agent@" + digB,
			dig:  digA,
			want: "registry.example/team/demo-agent@" + digA,
		},
		{
			name:    "malformed ref rejected",
			ref:     "not-a-ref",
			dig:     digA,
			wantErr: true,
		},
		{
			name:    "non-digest string rejected (must not become a tag)",
			ref:     "registry.example/team/demo-agent:v1",
			dig:     "v2",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PinnedRef(tc.ref, tc.dig)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("PinnedRef(%q, %q) = %q, nil; want error", tc.ref, tc.dig, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PinnedRef(%q, %q) unexpected error: %v", tc.ref, tc.dig, err)
			}
			if got != tc.want {
				t.Fatalf("PinnedRef(%q, %q) = %q; want %q", tc.ref, tc.dig, got, tc.want)
			}
		})
	}
}

func TestDefaultLocalName(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		want    string
		wantErr bool
	}{
		{name: "tag", ref: "registry.example/team/demo-agent:v1", want: "demo-agent.oap"},
		{name: "digest", ref: "registry.example/team/demo-agent@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"[:64], want: "demo-agent.oap"},
		{name: "no tag", ref: "registry.example/demo-agent", want: "demo-agent.oap"},
		{name: "port in host", ref: "localhost:5000/team/demo-agent:v1", want: "demo-agent.oap"},
		{name: "malformed: no slash", ref: "not-a-ref", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DefaultLocalName(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DefaultLocalName(%q) = %q, nil; want error", tc.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DefaultLocalName(%q) unexpected error: %v", tc.ref, err)
			}
			if got != tc.want {
				t.Fatalf("DefaultLocalName(%q) = %q; want %q", tc.ref, got, tc.want)
			}
		})
	}
}
