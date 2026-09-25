package agentsession_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// TestResolveSidecarImageRef pins the by-digest launch rewrite AND its local-dev
// exemption. On registry-backed clusters the sidecar container is rewritten to
// <repo>@<digest> so it launches the exact probed bytes. On local-dev-image
// clusters (local/desktop — images are `oap image load`ed by mutable tag and are
// NOT pullable by digest, mirroring `oap install --no-digest-pin`) the rewrite is
// SKIPPED: a `repo@sha256:…` ref there ErrImagePulls ("repository does not
// exist") and the sidecar container never starts.
func TestResolveSidecarImageRef(t *testing.T) {
	const digest = "sha256:2beaf520bd452c213ee8d32c491bd2d8cb33dd6c2c460fa4378548f5d4eae4a8"
	cases := []struct {
		name              string
		declared          string
		pinDigest         string
		usesLocalDevImage bool
		want              string
	}{
		{name: "registry cluster + tag + pin: rewrites to repo@digest", declared: "ghcr.io/x/y:v1", pinDigest: digest, usesLocalDevImage: false, want: "ghcr.io/x/y@" + digest},
		{name: "local-dev cluster + tag + pin: keeps the tag (digest unpullable)", declared: "pde-records-mcp:dev", pinDigest: digest, usesLocalDevImage: true, want: "pde-records-mcp:dev"},
		{name: "no pin baseline: keeps declared", declared: "ghcr.io/x/y:v1", pinDigest: "", usesLocalDevImage: false, want: "ghcr.io/x/y:v1"},
		{name: "already digest-qualified: untouched", declared: "ghcr.io/x/y@" + digest, pinDigest: digest, usesLocalDevImage: false, want: "ghcr.io/x/y@" + digest},
		{name: "empty declared: empty", declared: "", pinDigest: digest, usesLocalDevImage: false, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentsession.ResolveSidecarImageRef(tc.declared, tc.pinDigest, tc.usesLocalDevImage); got != tc.want {
				t.Errorf("ResolveSidecarImageRef(%q,%q,%v)=%q want %q", tc.declared, tc.pinDigest, tc.usesLocalDevImage, got, tc.want)
			}
		})
	}
}

func TestEnvPrefix(t *testing.T) {
	tests := map[string]string{
		"reddit-readonly": "REDDIT_READONLY",
		"echo":            "ECHO",
		"my_tool-1":       "MY_TOOL_1",
	}
	for in, want := range tests {
		if got := agentsession.EnvPrefix(in); got != want {
			t.Errorf("EnvPrefix(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAllocatePorts_NoCollisions(t *testing.T) {
	got := agentsession.AllocatePorts([]string{"a", "b", "c"})
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	seen := map[int32]bool{}
	for k, p := range got {
		if seen[p] {
			t.Errorf("port %d repeated for key %s", p, k)
		}
		seen[p] = true
	}
}

func TestAllocatePorts_Deterministic(t *testing.T) {
	a := agentsession.AllocatePorts([]string{"x", "y"})
	b := agentsession.AllocatePorts([]string{"x", "y"})
	if a["x"] != b["x"] || a["y"] != b["y"] {
		t.Errorf("port assignment not deterministic: %+v vs %+v", a, b)
	}
}

func TestBuildSidecarContainers_Image(t *testing.T) {
	rt := []spiceboxv1alpha1.ResolvedSidecarToolbox{{
		Name: "echo",
		Ref:  "echo",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/echo:v1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080, Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "p"},
		},
	}}
	containers, vols, err := agentsession.BuildSidecarContainers(agentsession.SidecarOpts{
		Resolved:   rt,
		SecretName: func(ref string) string { return "agentsession-x-toolbox-" + ref },
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(containers))
	}
	c := containers[0]
	if c.Name != "sidecar-echo" {
		t.Errorf("Name=%q", c.Name)
	}
	if c.Image != "ghcr.io/x/echo:v1" {
		t.Errorf("Image=%q", c.Image)
	}
	gotPort := false
	for _, e := range c.Env {
		if e.Name == "MCP_PORT" && e.Value == "18080" {
			gotPort = true
		}
	}
	if !gotPort {
		t.Errorf("expected MCP_PORT=18080 env, got %+v", c.Env)
	}
	if len(c.EnvFrom) == 0 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "agentsession-x-toolbox-echo" {
		t.Errorf("envFrom secretRef wrong: %+v", c.EnvFrom)
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot {
		t.Errorf("expected runAsNonRoot, got %+v", c.SecurityContext)
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("expected readOnlyRootFilesystem")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("expected allowPrivilegeEscalation=false")
	}
	if c.StartupProbe == nil || c.StartupProbe.HTTPGet == nil || c.StartupProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("startupProbe wrong: %+v", c.StartupProbe)
	}
	// Image variant emits two emptyDirs: the writable /tmp scratch (the
	// container runs read-only-rootfs and needs a usable tempdir) and the
	// ServiceAccount-token shadow that keeps this third-party image out of the
	// runner's Kubernetes identity.
	if len(vols) != 2 || vols[0].Name != "tmp-echo" || vols[0].EmptyDir == nil ||
		vols[1].Name != "no-sa-token-echo" || vols[1].EmptyDir == nil {
		t.Errorf("expected tmp-echo + no-sa-token-echo emptyDir volumes, got %+v", vols)
	}
	tmpMounted := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/tmp" && m.Name == "tmp-echo" {
			tmpMounted = true
		}
	}
	if !tmpMounted {
		t.Errorf("expected a writable /tmp mount, got %+v", c.VolumeMounts)
	}
}

func TestBuildSidecarContainers_Inline_AddsConfigMapVolume(t *testing.T) {
	rt := []spiceboxv1alpha1.ResolvedSidecarToolbox{{
		Name: "echo",
		Ref:  "echo",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source: spiceboxv1alpha1.SidecarToolboxSource{
				Inline: &spiceboxv1alpha1.SidecarToolboxInlineSource{
					BaseImage: "python:3.12-slim",
					Script: spiceboxv1alpha1.SidecarToolboxScriptSource{
						ConfigMapRef: spiceboxv1alpha1.SidecarToolboxConfigMapKeyRef{Name: "echo-mcp-src", Key: "server.py"},
					},
					Entrypoint: []string{"python", "/app/server.py"},
				},
			},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080, Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "p"},
		},
	}}
	containers, volumes, err := agentsession.BuildSidecarContainers(agentsession.SidecarOpts{
		Resolved:   rt,
		SecretName: func(ref string) string { return "s-" + ref },
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	c := containers[0]
	if c.Image != "python:3.12-slim" {
		t.Errorf("Image=%q", c.Image)
	}
	if !sliceEq(c.Command, []string{"python", "/app/server.py"}) {
		t.Errorf("Command=%v", c.Command)
	}
	if len(c.VolumeMounts) == 0 || c.VolumeMounts[0].MountPath != "/app" {
		t.Errorf("VolumeMounts wrong: %+v", c.VolumeMounts)
	}
	if c.VolumeMounts[0].ReadOnly != true {
		t.Errorf("VolumeMounts[0].ReadOnly should be true")
	}
	// Inline source emits its ConfigMap volume plus the writable /tmp scratch
	// and the ServiceAccount-token shadow.
	if len(volumes) != 3 || volumes[0].VolumeSource.ConfigMap == nil || volumes[0].VolumeSource.ConfigMap.Name != "echo-mcp-src" {
		t.Errorf("Volumes wrong: %+v", volumes)
	}
	if volumes[1].Name != "tmp-echo" || volumes[1].EmptyDir == nil {
		t.Errorf("expected tmp-echo emptyDir as second volume, got %+v", volumes)
	}
	if volumes[2].Name != "no-sa-token-echo" || volumes[2].EmptyDir == nil {
		t.Errorf("expected no-sa-token-echo emptyDir as third volume, got %+v", volumes)
	}
	_ = corev1.PullIfNotPresent // import-keep
}

// TestBuildSidecarContainers_ConfigEnv proves rt.Spec.Config flows through
// buildSidecarContainer (the unexported helper both run modes share) into the
// built container's AP_SIDECAR_CONFIG env var — BuildSidecarContainers is its
// only in-pod entry point.
func TestBuildSidecarContainers_ConfigEnv(t *testing.T) {
	rt := []spiceboxv1alpha1.ResolvedSidecarToolbox{{
		Name: "echo",
		Ref:  "echo",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/echo:v1"},
			Sandbox:      spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
			Transport:    spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080, Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "p"},
			Config:       `{"base_url":"https://example.com"}`,
		},
	}}
	containers, _, err := agentsession.BuildSidecarContainers(agentsession.SidecarOpts{
		Resolved:   rt,
		SecretName: func(ref string) string { return "agentsession-x-toolbox-" + ref },
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	c := containers[0]
	var gotConfig bool
	for _, e := range c.Env {
		if e.Name == "AP_SIDECAR_CONFIG" {
			gotConfig = true
			if e.Value != `{"base_url":"https://example.com"}` {
				t.Errorf("AP_SIDECAR_CONFIG value=%q", e.Value)
			}
		}
	}
	if !gotConfig {
		t.Errorf("expected AP_SIDECAR_CONFIG env, got %+v", c.Env)
	}
}

func TestBuildSidecarContainers_RequiresSecretNameFn(t *testing.T) {
	if _, _, err := agentsession.BuildSidecarContainers(agentsession.SidecarOpts{Resolved: nil, SecretName: nil}); err == nil {
		t.Fatal("expected error when SecretName fn is nil")
	}
}

func TestBuildSidecarContainers_BothSourceVariantsMissingErrors(t *testing.T) {
	rt := []spiceboxv1alpha1.ResolvedSidecarToolbox{{
		Name: "x",
		Ref:  "x",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			// Source intentionally empty
		},
	}}
	if _, _, err := agentsession.BuildSidecarContainers(agentsession.SidecarOpts{
		Resolved:   rt,
		SecretName: func(s string) string { return "s" },
	}); err == nil {
		t.Fatal("expected error when source has neither image nor inline")
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRunModeFor pins BOTH independent reasons RunModeFor returns
// RunModeSeparatePod -- secret-gating (any SecretInputs) and the declared
// Isolation=isolated field -- as separate rows, plus their combination and
// their absence. A table where only one row is load-bearing is exactly the
// shape this project has shipped broken before (see CLAUDE.md's "Ship gate"
// section): each row here was confirmed to fail on its own by temporarily
// reverting the branch it covers (see task-1-report.md for the transcript).
func TestRunModeFor(t *testing.T) {
	secretInputs := []spiceboxv1alpha1.SidecarToolboxSecretInput{
		{Name: "KUBECONFIG", Deliver: "file:/root/.kube/config", From: "kubeconfig"},
	}
	tests := []struct {
		name         string
		secretInputs []spiceboxv1alpha1.SidecarToolboxSecretInput
		isolation    spiceboxv1alpha1.SidecarToolboxIsolation
		want         string
	}{
		{
			name:         "secret-gated only: SecretInputs set, Isolation absent -> separate-pod",
			secretInputs: secretInputs,
			want:         agentsession.RunModeSeparatePod,
		},
		{
			name:      "isolated only: Isolation=isolated, no SecretInputs -> separate-pod",
			isolation: spiceboxv1alpha1.SidecarToolboxIsolationIsolated,
			want:      agentsession.RunModeSeparatePod,
		},
		{
			name:         "both: SecretInputs set AND Isolation=isolated -> separate-pod",
			secretInputs: secretInputs,
			isolation:    spiceboxv1alpha1.SidecarToolboxIsolationIsolated,
			want:         agentsession.RunModeSeparatePod,
		},
		{
			name: "neither: no SecretInputs, Isolation absent -> in-pod",
			want: agentsession.RunModeInPod,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := spiceboxv1alpha1.SidecarToolboxSpec{
				Source:       spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/echo:v1"},
				SecretInputs: tc.secretInputs,
				Isolation:    tc.isolation,
			}
			if got := agentsession.RunModeFor(spec); got != tc.want {
				t.Errorf("RunModeFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMergeNetwork_UnionsHostsSortedAndDeduped(t *testing.T) {
	mode, hosts := agentsession.MergeNetworkForTest(true,
		spiceboxv1alpha1.SpiceboxNetwork{Mode: spiceboxv1alpha1.NetworkModeAllowlist, AllowedHosts: []string{"a.example", "b.example", "a.example"}},
		spiceboxv1alpha1.SidecarToolboxNetwork{AllowedHosts: []string{"b.example", "c.example"}},
	)
	if mode != spiceboxv1alpha1.NetworkModeAllowlist {
		t.Errorf("mode=%q", mode)
	}
	want := []string{"a.example", "b.example", "c.example"}
	if !sliceEq(hosts, want) {
		t.Errorf("hosts=%v want %v", hosts, want)
	}
}

func TestMergeNetwork_UpgradesNoneToAllowlistWhenCRAddsHosts(t *testing.T) {
	mode, hosts := agentsession.MergeNetworkForTest(true,
		spiceboxv1alpha1.SpiceboxNetwork{Mode: spiceboxv1alpha1.NetworkModeNone},
		spiceboxv1alpha1.SidecarToolboxNetwork{AllowedHosts: []string{"reddit.com"}},
	)
	if mode != spiceboxv1alpha1.NetworkModeAllowlist {
		t.Errorf("mode should be upgraded; got %q", mode)
	}
	if !sliceEq(hosts, []string{"reddit.com"}) {
		t.Errorf("hosts=%v", hosts)
	}
}

func TestMergeNetwork_StaysAtNoneWhenBothEmpty(t *testing.T) {
	mode, hosts := agentsession.MergeNetworkForTest(true,
		spiceboxv1alpha1.SpiceboxNetwork{Mode: spiceboxv1alpha1.NetworkModeNone},
		spiceboxv1alpha1.SidecarToolboxNetwork{},
	)
	if mode != spiceboxv1alpha1.NetworkModeNone {
		t.Errorf("mode=%q", mode)
	}
	if len(hosts) != 0 {
		t.Errorf("hosts=%v", hosts)
	}
}

func TestMergeNetwork_FallsBackToCROnlyWhenClassMissing(t *testing.T) {
	mode, hosts := agentsession.MergeNetworkForTest(false,
		spiceboxv1alpha1.SpiceboxNetwork{Mode: spiceboxv1alpha1.NetworkModeAllowlist, AllowedHosts: []string{"class.example"}},
		spiceboxv1alpha1.SidecarToolboxNetwork{AllowedHosts: []string{"cr.example"}},
	)
	// Class hosts are dropped; CR alone drives.
	if !sliceEq(hosts, []string{"cr.example"}) {
		t.Errorf("hosts=%v want [cr.example]", hosts)
	}
	// Mode was empty (class ignored), upgraded by CR adding hosts.
	if mode != spiceboxv1alpha1.NetworkModeAllowlist {
		t.Errorf("mode=%q want allowlist (upgraded)", mode)
	}
}
