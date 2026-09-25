package oci

import "testing"

func TestGuardHost(t *testing.T) {
	cases := []struct {
		name      string
		ref       string
		allow     []string
		plainHTTP bool
		wantErr   bool
	}{
		{
			name:    "cloud metadata IP refused",
			ref:     "169.254.169.254/x/y",
			wantErr: true,
		},
		{
			name:      "cloud metadata IP refused even with PlainHTTP",
			ref:       "169.254.169.254/x/y",
			plainHTTP: true,
			wantErr:   true,
		},
		{
			name:    "metadata hostname refused",
			ref:     "metadata.google.internal/x",
			wantErr: true,
		},
		{
			name:      "metadata hostname refused even with PlainHTTP",
			ref:       "metadata.google.internal/x",
			plainHTTP: true,
			wantErr:   true,
		},
		{
			name:    "bare metadata hostname refused",
			ref:     "metadata/x",
			wantErr: true,
		},
		{
			name:    ".internal suffix refused",
			ref:     "svc.internal/x",
			wantErr: true,
		},
		{
			name:    ".local suffix refused",
			ref:     "printer.local/x",
			wantErr: true,
		},
		{
			name:    "private range refused without PlainHTTP",
			ref:     "10.1.2.3/x",
			wantErr: true,
		},
		{
			name:      "private range allowed with PlainHTTP",
			ref:       "10.1.2.3/x",
			plainHTTP: true,
			wantErr:   false,
		},
		{
			name:    "private range (192.168) refused without PlainHTTP",
			ref:     "192.168.0.5/x",
			wantErr: true,
		},
		{
			name:      "private range (192.168) allowed with PlainHTTP",
			ref:       "192.168.0.5/x",
			plainHTTP: true,
			wantErr:   false,
		},
		{
			name:    "loopback refused without PlainHTTP",
			ref:     "127.0.0.1:5000/x",
			wantErr: true,
		},
		{
			name:      "loopback allowed with PlainHTTP",
			ref:       "127.0.0.1:5000/x",
			plainHTTP: true,
			wantErr:   false,
		},
		{
			name:    "localhost refused without PlainHTTP",
			ref:     "localhost:5000/x",
			wantErr: true,
		},
		{
			name:      "localhost allowed with PlainHTTP",
			ref:       "localhost:5000/x",
			plainHTTP: true,
			wantErr:   false,
		},
		{
			name:    "public host allowed",
			ref:     "ghcr.io/acme/x:1",
			wantErr: false,
		},
		{
			name:    "public host allowed 2",
			ref:     "registry.example.com/x",
			wantErr: false,
		},
		{
			name:    "allow-listed private IP allowed even without PlainHTTP",
			ref:     "10.1.2.3/x",
			allow:   []string{"10.1.2.3"},
			wantErr: false,
		},
		{
			name:    "allow-listed host:port allowed",
			ref:     "internal-registry.internal:5000/x",
			allow:   []string{"internal-registry.internal:5000"},
			wantErr: false,
		},
		{
			name:    "metadata IP is NOT bypassed even if a different host is allow-listed",
			ref:     "169.254.169.254/x",
			allow:   []string{"ghcr.io"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardHost(tc.ref, tc.allow, tc.plainHTTP)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("guardHost(%q, %v, %v) = nil; want error", tc.ref, tc.allow, tc.plainHTTP)
				}
				return
			}
			if err != nil {
				t.Fatalf("guardHost(%q, %v, %v) unexpected error: %v", tc.ref, tc.allow, tc.plainHTTP, err)
			}
		})
	}
}
