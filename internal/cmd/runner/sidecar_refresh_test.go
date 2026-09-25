package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func refreshScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// sepPod is a small factory for a separate-pod ResolvedSidecarToolbox with a
// single allowlisted tool, used across the refresher tests.
func sepPod(name string, awaiting bool, ip string) spiceboxv1alpha1.ResolvedSidecarToolbox {
	return spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name:           name,
		Ref:            name + "-cr",
		Port:           9000,
		RunMode:        "separate-pod",
		AwaitingSecret: awaiting,
		SidecarPodIP:   ip,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:  name,
			Tools: []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
}

// liveEcho is the probe result matching sepPod's allowlist.
var liveEcho = []mcpprobe.Tool{{
	Name:        "echo",
	Description: "echoes input",
	InputSchema: []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`),
}}

// failedPod is sepPod with an operator-recorded terminal PodFailure — the
// shape of a sidecar whose pod crash-looped and therefore never got a PodIP.
func failedPod(name, reason, message string) spiceboxv1alpha1.ResolvedSidecarToolbox {
	rt := sepPod(name, false, "")
	rt.PodFailure = &spiceboxv1alpha1.SidecarPodFailure{Reason: reason, Message: message}
	return rt
}

// TestTerminallyFailedSidecars covers the selection rule behind the runner's
// "your tools are missing, and here is why" report.
//
// The dedup key is the FAILURE, not the sidecar: the refresher runs at the top
// of every turn, so keying on the sidecar name alone would either re-report the
// same crash on every turn (notice spam) or permanently suppress a DIFFERENT
// later failure on the same sidecar.
func TestTerminallyFailedSidecars(t *testing.T) {
	cases := []struct {
		name         string
		prevNotified map[string]string
		resolved     []spiceboxv1alpha1.ResolvedSidecarToolbox
		wantNames    []string
	}{
		{
			name:      "newly failed sidecar is reported",
			resolved:  []spiceboxv1alpha1.ResolvedSidecarToolbox{failedPod("k8s", "CrashLoopBackOff", "no cluster match")},
			wantNames: []string{"k8s"},
		},
		{
			name:         "same failure already reported is not repeated on the next turn",
			prevNotified: map[string]string{"k8s": "CrashLoopBackOff|no cluster match"},
			resolved:     []spiceboxv1alpha1.ResolvedSidecarToolbox{failedPod("k8s", "CrashLoopBackOff", "no cluster match")},
			wantNames:    nil,
		},
		{
			name:         "a DIFFERENT failure on an already-reported sidecar is reported again",
			prevNotified: map[string]string{"k8s": "CrashLoopBackOff|no cluster match"},
			resolved:     []spiceboxv1alpha1.ResolvedSidecarToolbox{failedPod("k8s", "CrashLoopBackOff", "permission denied reading kubeconfig")},
			wantNames:    []string{"k8s"},
		},
		{
			name:      "healthy sidecar is not reported",
			resolved:  []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("k8s", false, "10.0.0.1")},
			wantNames: nil,
		},
		{
			name:      "still-starting sidecar (no PodIP, no failure) is not reported",
			resolved:  []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("k8s", false, "")},
			wantNames: nil,
		},
		{
			name: "only the failed sidecar is reported when others are healthy",
			resolved: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				sepPod("ok", false, "10.0.0.1"),
				failedPod("bad", "ImagePullBackOff", `Back-off pulling image "ghcr.io/x:v1"`),
			},
			wantNames: []string{"bad"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := terminallyFailedSidecars(tc.prevNotified, tc.resolved)
			var names []string
			for _, rt := range got {
				names = append(names, rt.Name)
			}
			assert.Equal(t, tc.wantNames, names)
		})
	}
}

func TestNewlyReadySidecars(t *testing.T) {
	cases := []struct {
		name        string
		prevSynthed map[string]string
		resolved    []spiceboxv1alpha1.ResolvedSidecarToolbox
		wantNames   []string
	}{
		{
			name:        "awaiting-secret separate-pod: not yet ready",
			prevSynthed: map[string]string{},
			resolved:    []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("gated", true, "")},
			wantNames:   nil,
		},
		{
			name:        "no-secret but IP not reflected: not yet ready",
			prevSynthed: map[string]string{},
			resolved:    []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("gated", false, "")},
			wantNames:   nil,
		},
		{
			name:        "ready and unsynthesized: yields it",
			prevSynthed: map[string]string{},
			resolved:    []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("gated", false, "10.0.0.7")},
			wantNames:   []string{"gated"},
		},
		{
			name:        "ready but already synthesized at same IP: skipped",
			prevSynthed: map[string]string{"gated": "10.0.0.7"},
			resolved:    []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("gated", false, "10.0.0.7")},
			wantNames:   nil,
		},
		{
			name:        "synthesized but pod IP changed (replaced pod): yields it again",
			prevSynthed: map[string]string{"gated": "10.0.0.7"},
			resolved:    []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("gated", false, "10.0.0.99")},
			wantNames:   []string{"gated"},
		},
		{
			name:        "in-pod sidecar is never returned (boot-only path)",
			prevSynthed: map[string]string{},
			resolved: []spiceboxv1alpha1.ResolvedSidecarToolbox{{
				Name: "inpod", Ref: "inpod-cr", Port: 9000, RunMode: "in-pod",
			}},
			wantNames: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newlyReadySidecars(tc.prevSynthed, tc.resolved)
			var names []string
			for _, rt := range got {
				names = append(names, rt.Name)
			}
			assert.Equal(t, tc.wantNames, names)
		})
	}
}

// TestNewlyReadySidecars_AwaitingThenReadyAcrossCalls models the headline
// same-session flow: a separate-pod sidecar flips from awaiting→ready across
// two refresher passes and is yielded exactly once (the caller marks it
// synthesized between calls, mirroring the refresher's own bookkeeping).
func TestNewlyReadySidecars_AwaitingThenReadyAcrossCalls(t *testing.T) {
	synthed := map[string]string{}

	// Pass 1: still awaiting the secret — nothing ready.
	awaiting := []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("gated", true, "")}
	first := newlyReadySidecars(synthed, awaiting)
	require.Empty(t, first, "awaiting sidecar must not be ready on the first pass")

	// Pass 2: secret produced, pod Ready, IP reflected — now yielded once.
	ready := []spiceboxv1alpha1.ResolvedSidecarToolbox{sepPod("gated", false, "10.0.0.7")}
	second := newlyReadySidecars(synthed, ready)
	require.Len(t, second, 1, "ready sidecar must be yielded on the second pass")
	assert.Equal(t, "gated", second[0].Name)

	// Caller records the synthesized pod IP (the refresher does this internally).
	synthed[second[0].Name] = second[0].SidecarPodIP

	// Pass 3: same ready state — must NOT be yielded again (dedup).
	third := newlyReadySidecars(synthed, ready)
	assert.Empty(t, third, "an already-synthesized sidecar must not be re-yielded")
}

// TestSidecarToolRefresher_TerminalFailureNotifiesOnceAndAdvises is the
// end-to-end guard on the silent-degradation bug.
//
// A crash-looping sidecar never gets a PodIP, so it never enters the
// probe/synthesize path — before this change the refresher returned nothing and
// said nothing, and the agent completed its turn without the toolset it was
// promised. The refresher must now surface it BOTH ways: a deterministic
// user-visible notice (which does not depend on the model choosing to relay it)
// and an advisory injected into the agent's context (so it can explain and stop
// rather than proceeding tool-less).
//
// It must also fire exactly once for one failure, because the refresher runs at
// the top of EVERY turn.
func TestSidecarToolRefresher_TerminalFailureNotifiesOnceAndAdvises(t *testing.T) {
	scheme := refreshScheme(t)
	const logTail = `no cluster match for "demo-cluster.abc123" in region "us-east-1"`

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				failedPod("k8s", "CrashLoopBackOff", logTail),
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	key := client.ObjectKeyFromObject(sess)

	probeCalls := 0
	probe := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		probeCalls++
		return liveEcho, nil
	}

	var notices []*notice.Notice
	notify := func(_ context.Context, n *notice.Notice) { notices = append(notices, n) }

	refresh := newSidecarToolRefresher(c, key, probe, map[string]string{}, nil, nil, nil, notify)

	// Turn 1: the failure is reported both ways.
	res, err := refresh(context.Background())
	require.NoError(t, err, "a terminal sidecar failure is reported, not returned as a refresher error")
	assert.Empty(t, res.Added, "a crash-looping sidecar contributes no tools")
	assert.Equal(t, 0, probeCalls, "a pod with no IP must never be probed")

	require.Len(t, notices, 1, "exactly one user-visible notice for one failure")
	assert.Contains(t, notices[0].Args().Lead, "k8s", "the notice must name which sidecar is unavailable")
	require.NotNil(t, notices[0].Args().Excerpt, "the cause must reach the user")
	assert.Contains(t, notices[0].Args().Excerpt.Content, logTail,
		"the actionable cause rides the excerpt, where every surface renders it inert")

	require.Len(t, res.Advisories, 1, "exactly one advisory injected into the agent's context")
	assert.Contains(t, res.Advisories[0], "k8s", "the advisory must name the unavailable sidecar")
	assert.Contains(t, res.Advisories[0], logTail, "the advisory must carry the cause the agent explains")

	// Turn 2: same failure, still crash-looping. Must NOT repeat — the refresher
	// runs every turn and would otherwise spam the channel for the whole session.
	res, err = refresh(context.Background())
	require.NoError(t, err)
	assert.Len(t, notices, 1, "the same failure must not re-notify on a later turn")
	assert.Empty(t, res.Advisories, "the same failure must not re-advise on a later turn")
}

// TestSidecarToolRefresher_TerminalFailureSurvivesRunnerRestart pins the
// durable half of the once-per-failure contract.
//
// The runner pod runs with RestartPolicy=OnFailure, so an OOM kill or a panic
// restarts the container in place, mid-session. The separate sidecar pod is
// untouched — a wrong image tag sits in ImagePullBackOff forever, and the
// operator keeps writing the identical PodFailure — but the fresh runner
// process builds an empty `notified` map and re-reports the identical warning
// into a thread where the user already read it and moved on. Same on every
// idle-sleep → wake cycle for the life of the session.
func TestSidecarToolRefresher_TerminalFailureSurvivesRunnerRestart(t *testing.T) {
	scheme := refreshScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				failedPod("k8s", "ImagePullBackOff", "manifest unknown"),
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	key := client.ObjectKeyFromObject(sess)
	probe := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) { return liveEcho, nil }

	var notices []*notice.Notice
	notify := func(_ context.Context, n *notice.Notice) { notices = append(notices, n) }

	// A fresh runner process: nothing carried over in memory.
	restarted := func() func(context.Context) (runner.ToolRefreshResult, error) {
		return newSidecarToolRefresher(c, key, probe, map[string]string{}, nil, nil, nil, notify)
	}

	_, err := restarted()(context.Background())
	require.NoError(t, err)
	require.Len(t, notices, 1, "the failure is reported once")

	_, err = restarted()(context.Background())
	require.NoError(t, err)

	assert.Len(t, notices, 1,
		"a runner restart must not re-report a sidecar failure the user already saw")
}

// A failure the user has NOT seen must still reach them after a restart: the
// dedup is per-FAILURE, and losing that distinction would permanently mask a
// different later crash of the same sidecar.
func TestSidecarToolRefresher_DifferentFailureReportedAfterRunnerRestart(t *testing.T) {
	scheme := refreshScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				failedPod("k8s", "ImagePullBackOff", "manifest unknown"),
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	key := client.ObjectKeyFromObject(sess)
	probe := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) { return liveEcho, nil }
	var notices []*notice.Notice
	notify := func(_ context.Context, n *notice.Notice) { notices = append(notices, n) }

	_, err := newSidecarToolRefresher(c, key, probe, map[string]string{}, nil, nil, nil, notify)(context.Background())
	require.NoError(t, err)
	require.Len(t, notices, 1)

	// The runner restarts, and the sidecar is now failing for a different reason.
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), key, &live))
	live.Status.ResolvedSidecarToolboxes = []spiceboxv1alpha1.ResolvedSidecarToolbox{
		failedPod("k8s", "CrashLoopBackOff", "exit 1"),
	}
	require.NoError(t, c.Status().Update(context.Background(), &live))

	_, err = newSidecarToolRefresher(c, key, probe, map[string]string{}, nil, nil, nil, notify)(context.Background())
	require.NoError(t, err)

	assert.Len(t, notices, 2, "a genuinely different failure must be reported even across a restart")
}

// TestSidecarToolRefresher_RecoveredSidecarSynthesizesAndCanReFail verifies the
// failure record does not wedge a sidecar out of service: once the operator
// clears PodFailure and reflects a PodIP, the refresher synthesizes the tools
// normally — and a LATER failure is reported again rather than being suppressed
// by the first one's fingerprint.
func TestSidecarToolRefresher_RecoveredSidecarSynthesizesAndCanReFail(t *testing.T) {
	scheme := refreshScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				failedPod("k8s", "CrashLoopBackOff", "boom"),
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	key := client.ObjectKeyFromObject(sess)

	probe := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) { return liveEcho, nil }
	var notices []*notice.Notice
	notify := func(_ context.Context, n *notice.Notice) { notices = append(notices, n) }

	refresh := newSidecarToolRefresher(c, key, probe, map[string]string{}, nil, nil, nil, notify)

	res, err := refresh(context.Background())
	require.NoError(t, err)
	require.Len(t, notices, 1, "the first failure is reported")
	require.Empty(t, res.Added)

	// Operator clears the failure and reflects an IP (the pod recovered).
	setResolved := func(rt spiceboxv1alpha1.ResolvedSidecarToolbox) {
		var live spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(context.Background(), key, &live))
		live.Status.ResolvedSidecarToolboxes = []spiceboxv1alpha1.ResolvedSidecarToolbox{rt}
		require.NoError(t, c.Status().Update(context.Background(), &live))
	}
	setResolved(sepPod("k8s", false, "10.0.0.7"))

	res, err = refresh(context.Background())
	require.NoError(t, err)
	assert.Len(t, res.Added, 1, "a recovered sidecar must synthesize its tools normally")

	// It crashes again with the SAME cause as before. Because it recovered in
	// between, this is a NEW failure and must be reported again.
	setResolved(failedPod("k8s", "CrashLoopBackOff", "boom"))

	res, err = refresh(context.Background())
	require.NoError(t, err)
	assert.Len(t, notices, 2, "a failure after a recovery must be reported again, not suppressed")
	assert.Len(t, res.Advisories, 1, "and re-advised to the agent")
}

// TestSidecarToolRefresher_AppendsOnceNoDuplicate drives the full refresher
// closure against a fake client whose AgentSession status flips a separate-pod
// sidecar from awaiting→ready between two calls. The first call (awaiting)
// returns nothing; the second (ready) probes+synthesizes and returns the tool;
// a third call (status unchanged) returns nothing — proving no duplication.
func TestSidecarToolRefresher_AppendsOnceNoDuplicate(t *testing.T) {
	scheme := refreshScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				sepPod("gated", true, ""), // awaiting at first
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	key := client.ObjectKeyFromObject(sess)

	probeCalls := 0
	probe := func(_ context.Context, url string) ([]mcpprobe.Tool, error) {
		probeCalls++
		assert.Equal(t, "http://10.0.0.7:9000", url, "probe must target the pod IP URL")
		return liveEcho, nil
	}

	refresh := newSidecarToolRefresher(c, key, probe, map[string]string{}, nil, nil, nil, nil)

	// Call 1: still awaiting — nothing added, no probe.
	res, err := refresh(context.Background())
	added := res.Added
	require.NoError(t, err)
	assert.Empty(t, added, "awaiting sidecar yields no tools")
	assert.Equal(t, 0, probeCalls, "no probe while awaiting")

	// Flip the status to Ready (secret produced, pod up, IP reflected).
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), key, &live))
	live.Status.ResolvedSidecarToolboxes = []spiceboxv1alpha1.ResolvedSidecarToolbox{
		sepPod("gated", false, "10.0.0.7"),
	}
	require.NoError(t, c.Status().Update(context.Background(), &live))

	// Call 2: ready — probes once, synthesizes, returns the tool.
	res, err = refresh(context.Background())
	added = res.Added
	require.NoError(t, err)
	require.Len(t, added, 1, "ready sidecar must synthesize exactly one tool")
	assert.Equal(t, 1, probeCalls, "exactly one probe for the newly-ready sidecar")

	// Call 3: unchanged status — must NOT re-probe or re-add (dedup).
	res, err = refresh(context.Background())
	added = res.Added
	require.NoError(t, err)
	assert.Empty(t, added, "an already-synthesized sidecar must not be re-added")
	assert.Equal(t, 1, probeCalls, "no additional probe on the dedup pass")
}

// TestSidecarToolRefresher_ResynthesizesOnPodIPChange verifies that when an
// already-synthesized separate-pod sidecar's SidecarPodIP changes (the operator
// replaced the pod after a token rotation), the refresher RE-probes the new pod
// and returns the freshly synthesized tools so the loop can replace the stale
// ones. Dedup is by (name → SidecarPodIP): a bare name match is no longer
// sufficient to skip.
func TestSidecarToolRefresher_ResynthesizesOnPodIPChange(t *testing.T) {
	scheme := refreshScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				sepPod("gated", false, "10.0.0.7"), // ready at the old IP
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	key := client.ObjectKeyFromObject(sess)

	var probedURLs []string
	probe := func(_ context.Context, url string) ([]mcpprobe.Tool, error) {
		probedURLs = append(probedURLs, url)
		return liveEcho, nil
	}

	// Seed the synthed set as if boot synthesized "gated" at the OLD IP.
	synthed := map[string]string{"gated": "10.0.0.7"}
	refresh := newSidecarToolRefresher(c, key, probe, synthed, nil, nil, nil, nil)

	// Call 1: status unchanged (same IP) — must NOT re-probe or re-add.
	res, err := refresh(context.Background())
	added := res.Added
	require.NoError(t, err)
	assert.Empty(t, added, "unchanged sidecar must not be re-synthesized")
	assert.Empty(t, probedURLs, "no probe when the pod IP is unchanged")

	// Operator replaced the pod: the SidecarPodIP changes to a NEW address.
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), key, &live))
	live.Status.ResolvedSidecarToolboxes = []spiceboxv1alpha1.ResolvedSidecarToolbox{
		sepPod("gated", false, "10.0.0.99"), // new pod, new IP
	}
	require.NoError(t, c.Status().Update(context.Background(), &live))

	// Call 2: IP changed — re-probe the NEW pod and return its tools.
	res, err = refresh(context.Background())
	added = res.Added
	require.NoError(t, err)
	require.Len(t, added, 1, "IP change must re-synthesize the sidecar's tool")
	require.Len(t, probedURLs, 1, "exactly one re-probe for the replaced pod")
	assert.Equal(t, "http://10.0.0.99:9000", probedURLs[0], "re-probe must target the NEW pod IP")
	assert.Equal(t, "10.0.0.99", synthed["gated"], "synthed set must record the new pod IP")

	// Call 3: status unchanged at the new IP — must NOT re-probe again (dedup).
	res, err = refresh(context.Background())
	added = res.Added
	require.NoError(t, err)
	assert.Empty(t, added, "an already-synthesized sidecar at the same IP must not be re-added")
	assert.Len(t, probedURLs, 1, "no additional probe on the dedup pass")
}

// TestSidecarToolRefresher_ProbeFailureSurfacedAndRetried verifies that a probe
// failure on a newly-ready sidecar is surfaced as an error (never silently
// dropped) and that the sidecar stays out of the synthesized set so a later
// refresh retries it.
func TestSidecarToolRefresher_ProbeFailureSurfacedAndRetried(t *testing.T) {
	scheme := refreshScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{
				sepPod("gated", false, "10.0.0.7"), // ready
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	key := client.ObjectKeyFromObject(sess)

	failNext := true
	probe := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		if failNext {
			return nil, errors.New("connection refused")
		}
		return liveEcho, nil
	}

	refresh := newSidecarToolRefresher(c, key, probe, map[string]string{}, nil, nil, nil, nil)

	// Call 1: probe fails — error surfaced, no tools added.
	res, err := refresh(context.Background())
	added := res.Added
	require.Error(t, err, "probe failure must be surfaced, not swallowed")
	assert.Empty(t, added)

	// Call 2: probe now succeeds — the sidecar was NOT marked synthesized, so
	// it is retried and now yields its tool.
	failNext = false
	res, err = refresh(context.Background())
	added = res.Added
	require.NoError(t, err)
	require.Len(t, added, 1, "previously-failed sidecar must be retried and synthesized")
}

// TestSidecarToolRefresher_ProvenanceStamped verifies that when an image
// ObservedPin is present in the AgentSession status and provenance is wired,
// the refresher stamps the image pin onto each synthesized tool's provenance
// record. This mirrors the boot-path provenance logic in run().
func TestSidecarToolRefresher_ProvenanceStamped(t *testing.T) {
	scheme := refreshScheme(t)
	imageDigest := "sha256:abc123"
	rt := sepPod("gated", false, "10.0.0.7")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{rt},
			// ObservedPins written by the agentsession controller at session start.
			ObservedPins: []spiceboxv1alpha1.ObservedPin{
				{
					Name: rt.Ref, // "gated-cr"
					Pin: spiceboxv1alpha1.PinRecord{
						Kind:     imagepin.KindName,
						Strength: "frozen",
						Digest:   imageDigest,
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	key := client.ObjectKeyFromObject(sess)

	probe := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		return liveEcho, nil
	}
	provenance := runner.NewToolProvenance()
	refresh := newSidecarToolRefresher(c, key, probe, map[string]string{}, provenance, nil, nil, nil)

	res, err := refresh(context.Background())
	added := res.Added
	require.NoError(t, err)
	require.Len(t, added, 1, "sidecar must synthesize its tool")

	rec, ok := provenance.Get(added[0].Name())
	require.True(t, ok, "provenance must be recorded for the synthesized tool")
	assert.Equal(t, imagepin.KindName, rec.Pin.Kind, "pin kind must be image")
	assert.Equal(t, imageDigest, rec.Pin.Digest, "pin digest must match ObservedPin")
	assert.Equal(t, rt.Ref, rec.Name, "provenance name must be the SidecarToolbox CR ref")
}
