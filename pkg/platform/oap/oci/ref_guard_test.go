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
			name:  "allow-list match is case-insensitive",
			ref:   "Registry.Corp.Internal/x",
			allow: []string{"registry.corp.internal"},
		},
		{
			name:    "metadata IP is NOT bypassed even if a different host is allow-listed",
			ref:     "169.254.169.254/x",
			allow:   []string{"ghcr.io"},
			wantErr: true,
		},
		// Bracketed IPv6 with no port: net.SplitHostPort rejects it, and these
		// used to pass as public hostnames with the brackets still on.
		{name: "bracketed link-local IPv6, no port: refused", ref: "[fe80::1]/x", wantErr: true},
		{name: "bracketed link-local IPv6, no port: refused even with PlainHTTP", ref: "[fe80::1]/x", plainHTTP: true, wantErr: true},
		{name: "IPv4-mapped metadata IP, no port: refused even with PlainHTTP", ref: "[::ffff:169.254.169.254]/x", plainHTTP: true, wantErr: true},
		{name: "IPv4-mapped metadata IP with port: refused", ref: "[::ffff:169.254.169.254]:80/x", plainHTTP: true, wantErr: true},
		{name: "bracketed ULA IPv6, no port: refused", ref: "[fd00::1]/x", wantErr: true},
		{name: "bracketed ULA IPv6, no port: allowed with PlainHTTP", ref: "[fd00::1]/x", plainHTTP: true},
		{name: "IPv4-mapped private IP, no port: refused", ref: "[::ffff:10.0.0.1]/x", wantErr: true},
		{name: "bracketed IPv6 loopback, no port: refused", ref: "[::1]/x", wantErr: true},
		{name: "bracketed IPv6 loopback, no port: allowed with PlainHTTP", ref: "[::1]/x", plainHTTP: true},
		{name: "bracketed public IPv6, no port: allowed", ref: "[2606:4700::1]/x"},
		// The unspecified address dials the local host.
		{name: "unspecified IPv4: refused even with PlainHTTP", ref: "0.0.0.0:5000/x", plainHTTP: true, wantErr: true},
		{name: "unspecified IPv6: refused even with PlainHTTP", ref: "[::]:5000/x", plainHTTP: true, wantErr: true},
		{name: "unspecified IPv4: allowed when allow-listed", ref: "0.0.0.0:5000/x", allow: []string{"0.0.0.0:5000"}},
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
