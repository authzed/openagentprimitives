package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestSidecarProbeURL(t *testing.T) {
	cases := []struct {
		name      string
		rt        spiceboxv1alpha1.ResolvedSidecarToolbox
		envLookup func(string) string
		wantURL   string
		wantReady bool
	}{
		// AwaitingSecret: always skip, regardless of other fields.
		{
			name: "separate-pod/AwaitingSecret=true: skip",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name:           "mytool",
				RunMode:        "separate-pod",
				AwaitingSecret: true,
				SidecarPodIP:   "10.0.0.5",
				Port:           9000,
			},
			envLookup: func(string) string { return "" },
			wantURL:   "",
			wantReady: false,
		},
		// separate-pod, not awaiting but IP not yet reflected.
		{
			name: "separate-pod/no IP yet: not ready",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name:           "mytool",
				RunMode:        "separate-pod",
				AwaitingSecret: false,
				SidecarPodIP:   "",
				Port:           9000,
			},
			envLookup: func(string) string { return "" },
			wantURL:   "",
			wantReady: false,
		},
		// separate-pod, IP present: build http://<IP>:<port>.
		{
			name: "separate-pod/IP ready: build pod-IP URL",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name:           "mytool",
				RunMode:        "separate-pod",
				AwaitingSecret: false,
				SidecarPodIP:   "10.0.0.5",
				Port:           9000,
			},
			envLookup: func(string) string { return "" },
			wantURL:   "http://10.0.0.5:9000",
			wantReady: true,
		},
		// in-pod (default/empty RunMode): read env.
		{
			name: "in-pod: env present",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name:    "echo",
				RunMode: "",
				Port:    0, // will be overwritten by caller after parse
			},
			// env lookup returns the port as a string; caller parses and sets rt.Port.
			// sidecarProbeURL receives the already-set Port (from env); we pass a
			// lookup that returns "3456" so the function can read it.
			envLookup: func(name string) string {
				if name == "MCP_PORT_ECHO" {
					return "3456"
				}
				return ""
			},
			wantURL:   "http://127.0.0.1:3456",
			wantReady: true,
		},
		// in-pod: env missing → not ready.
		{
			name: "in-pod: env missing",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name:    "echo",
				RunMode: "",
			},
			envLookup: func(string) string { return "" },
			wantURL:   "",
			wantReady: false,
		},
		// in-pod (explicit "in-pod" RunMode string): same as default.
		{
			name: "in-pod explicit: env present",
			rt: spiceboxv1alpha1.ResolvedSidecarToolbox{
				Name:    "files",
				RunMode: "in-pod",
			},
			envLookup: func(name string) string {
				if name == "MCP_PORT_FILES" {
					return "4000"
				}
				return ""
			},
			wantURL:   "http://127.0.0.1:4000",
			wantReady: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotReady := sidecarProbeURL(tc.rt, tc.envLookup)
			assert.Equal(t, tc.wantURL, gotURL, "URL")
			assert.Equal(t, tc.wantReady, gotReady, "ready")
		})
	}
}
