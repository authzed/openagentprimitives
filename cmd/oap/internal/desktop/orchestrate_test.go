package desktop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

// validKubeconfigYAML is a minimal parseable kubeconfig (matches the
// fixture shape in netforward_test.go) so RewriteKubeconfigServer
// succeeds inside Up.
const validKubeconfigYAML = `apiVersion: v1
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: default
`

// fakeProvider is a fake desktop.ClusterProvider that records the order
// steps are invoked in, so orchestration tests can assert sequencing
// (including hook steps, appended by the tests themselves onto calls).
type fakeProvider struct {
	calls           []string
	ip              string
	failOn          string
	gotKubeconfigIP string // the ip Engine.Up passed into Kubeconfig, for asserting it's the GuestIP result
}

func (f *fakeProvider) Provision(context.Context) error {
	f.calls = append(f.calls, "provision")
	return f.errIf("provision")
}
func (f *fakeProvider) Start(context.Context) error {
	f.calls = append(f.calls, "start")
	return f.errIf("start")
}
func (f *fakeProvider) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	return f.errIf("stop")
}
func (f *fakeProvider) Destroy(context.Context) error {
	f.calls = append(f.calls, "destroy")
	return f.errIf("destroy")
}
func (f *fakeProvider) Status(context.Context) (desktop.State, error) {
	return desktop.StateRunning, nil
}
func (f *fakeProvider) GuestIP(context.Context) (string, error) {
	f.calls = append(f.calls, "guestip")
	return f.ip, f.errIf("guestip")
}
func (f *fakeProvider) Kubeconfig(_ context.Context, ip string) ([]byte, error) {
	f.calls = append(f.calls, "kubeconfig")
	f.gotKubeconfigIP = ip
	if f.errIf("kubeconfig") != nil {
		return nil, f.errIf("kubeconfig")
	}
	return []byte(validKubeconfigYAML), nil
}
func (f *fakeProvider) errIf(step string) error {
	if f.failOn == step {
		return errors.New(step + " failed")
	}
	return nil
}

var _ desktop.ClusterProvider = (*fakeProvider)(nil)

func validConfig() desktop.Config {
	return desktop.Config{Model: desktop.ModelConfig{Provider: "anthropic", APIKey: "k"}}
}

func TestUp_FullSequence_WithExternalChannel(t *testing.T) {
	// "testchannel" is a test-only kind, registered only for this test —
	// no real channel backend exists for it. The production
	// knownExternalChannelKinds map (config.go) stays empty until a real
	// channelkinds backend lands, keeping Config.Validate() fail-closed.
	desktop.RegisterTestChannelKind(t, "testchannel")
	fp := &fakeProvider{ip: "192.168.64.7"}
	var gotEnv map[string]string
	var gotCfg desktop.Config
	eng := desktop.NewEngine(fp, desktop.EngineHooks{
		Install: func(_ context.Context, kubeconfig []byte, env map[string]string) error {
			fp.calls = append(fp.calls, "install")
			gotEnv = env
			assert.Contains(t, string(kubeconfig), "192.168.64.7", "install must see the IP-rewritten kubeconfig")
			return nil
		},
		Configure: func(_ context.Context, kubeconfig []byte, cfg desktop.Config) error {
			fp.calls = append(fp.calls, "configure")
			gotCfg = cfg
			assert.Contains(t, string(kubeconfig), "192.168.64.7", "configure must see the IP-rewritten kubeconfig")
			return nil
		},
		InitLocal: func(_ context.Context, _ []byte, _ map[string]string) (string, error) {
			fp.calls = append(fp.calls, "initlocal")
			return "https://x.ngrok.app", nil
		},
	})

	cfg := desktop.Config{
		Model:   desktop.ModelConfig{Provider: "anthropic", APIKey: "k"},
		Channel: &desktop.ChannelConfig{Kind: "testchannel", Credentials: map[string]string{"token": "wa-x"}},
		Ngrok:   &desktop.NgrokConfig{AuthToken: "t"},
	}
	url, err := eng.Up(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, "https://x.ngrok.app", url)
	assert.Equal(t, []string{"provision", "start", "guestip", "kubeconfig", "install", "configure", "initlocal"}, fp.calls,
		"install then configure then (Channel != nil) initlocal, after provider bring-up")
	assert.Equal(t, "192.168.64.7", fp.gotKubeconfigIP, "Kubeconfig must receive the same ip GuestIP resolved")
	assert.Equal(t, "k", gotEnv["ANTHROPIC_API_KEY"])
	assert.Equal(t, "t", gotEnv["NGROK_AUTHTOKEN"])
	require.NotNil(t, gotCfg.Channel)
	assert.Equal(t, "testchannel", gotCfg.Channel.Kind, "Configure receives the full Config, not just env")
}

func TestUp_SkipsInitLocal_WhenChannelNil(t *testing.T) {
	fp := &fakeProvider{ip: "192.168.64.7"}
	eng := desktop.NewEngine(fp, desktop.EngineHooks{
		Install: func(context.Context, []byte, map[string]string) error {
			fp.calls = append(fp.calls, "install")
			return nil
		},
		Configure: func(context.Context, []byte, desktop.Config) error {
			fp.calls = append(fp.calls, "configure")
			return nil
		},
		InitLocal: func(context.Context, []byte, map[string]string) (string, error) {
			t.Fatal("InitLocal must not run for the built-in chat (Channel == nil)")
			return "", nil
		},
	})

	url, err := eng.Up(context.Background(), validConfig())
	require.NoError(t, err)
	assert.Empty(t, url, "built-in chat has no public URL")
	assert.Equal(t, []string{"provision", "start", "guestip", "kubeconfig", "install", "configure"}, fp.calls)
}

func TestUp_AbortsOnProvisionError(t *testing.T) {
	fp := &fakeProvider{failOn: "provision"}
	eng := desktop.NewEngine(fp, desktop.EngineHooks{
		Install: func(context.Context, []byte, map[string]string) error {
			t.Fatal("Install must not run when Provision fails")
			return nil
		},
	})
	_, err := eng.Up(context.Background(), validConfig())
	require.Error(t, err)
	assert.Equal(t, []string{"provision"}, fp.calls, "stops immediately; no start/guestip/kubeconfig")
}

func TestUp_AbortsOnStartError(t *testing.T) {
	fp := &fakeProvider{failOn: "start"}
	_, err := desktop.NewEngine(fp, desktop.EngineHooks{}).Up(context.Background(), validConfig())
	require.Error(t, err)
	assert.Equal(t, []string{"provision", "start"}, fp.calls)
}

func TestUp_AbortsOnEmptyGuestIP(t *testing.T) {
	// GuestIP succeeds but returns "" (e.g. the vsock handshake never
	// landed); RewriteKubeconfigServer rejects an empty IP.
	fp := &fakeProvider{ip: ""}
	_, err := desktop.NewEngine(fp, desktop.EngineHooks{}).Up(context.Background(), validConfig())
	require.Error(t, err)
	assert.Equal(t, []string{"provision", "start", "guestip", "kubeconfig"}, fp.calls)
}

func TestUp_AbortsOnInstallError(t *testing.T) {
	fp := &fakeProvider{ip: "192.168.64.7"}
	wantErr := errors.New("install boom")
	eng := desktop.NewEngine(fp, desktop.EngineHooks{
		Install: func(context.Context, []byte, map[string]string) error {
			fp.calls = append(fp.calls, "install")
			return wantErr
		},
		Configure: func(context.Context, []byte, desktop.Config) error {
			t.Fatal("Configure must not run when Install fails")
			return nil
		},
	})
	_, err := eng.Up(context.Background(), validConfig())
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, []string{"provision", "start", "guestip", "kubeconfig", "install"}, fp.calls)
}

func TestUp_AbortsOnConfigureError(t *testing.T) {
	// Channel is populated (via the test-only kind) so this test also
	// proves InitLocal stays gated behind a successful Configure even
	// when cfg.Channel != nil, not just when the built-in chat is used.
	desktop.RegisterTestChannelKind(t, "testchannel")
	fp := &fakeProvider{ip: "192.168.64.7"}
	wantErr := errors.New("configure boom")
	eng := desktop.NewEngine(fp, desktop.EngineHooks{
		Install: func(context.Context, []byte, map[string]string) error {
			fp.calls = append(fp.calls, "install")
			return nil
		},
		Configure: func(context.Context, []byte, desktop.Config) error {
			fp.calls = append(fp.calls, "configure")
			return wantErr
		},
		InitLocal: func(context.Context, []byte, map[string]string) (string, error) {
			t.Fatal("InitLocal must not run when Configure fails")
			return "", nil
		},
	})
	_, err := eng.Up(context.Background(), desktop.Config{
		Model:   desktop.ModelConfig{Provider: "anthropic", APIKey: "k"},
		Channel: &desktop.ChannelConfig{Kind: "testchannel"},
	})
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, []string{"provision", "start", "guestip", "kubeconfig", "install", "configure"}, fp.calls)
}

func TestUp_AbortsOnInvalidConfig(t *testing.T) {
	fp := &fakeProvider{}
	_, err := desktop.NewEngine(fp, desktop.EngineHooks{}).Up(context.Background(), desktop.Config{})
	require.Error(t, err, "missing model must abort before touching the provider")
	assert.Empty(t, fp.calls, "provider must not be touched when config validation fails")
}

func TestUp_NilHooksAreSkippedWithoutPanic(t *testing.T) {
	fp := &fakeProvider{ip: "192.168.64.7"}
	url, err := desktop.NewEngine(fp, desktop.EngineHooks{}).Up(context.Background(), validConfig())
	require.NoError(t, err)
	assert.Empty(t, url)
}

func TestDown_CallsProviderStop(t *testing.T) {
	fp := &fakeProvider{}
	require.NoError(t, desktop.NewEngine(fp, desktop.EngineHooks{}).Down(context.Background()))
	assert.Equal(t, []string{"stop"}, fp.calls)
}

func TestUninstall_CallsProviderDestroy(t *testing.T) {
	fp := &fakeProvider{}
	require.NoError(t, desktop.NewEngine(fp, desktop.EngineHooks{}).Uninstall(context.Background()))
	assert.Equal(t, []string{"destroy"}, fp.calls)
}

func TestStatus_DelegatesToProvider(t *testing.T) {
	fp := &fakeProvider{}
	st, err := desktop.NewEngine(fp, desktop.EngineHooks{}).Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, desktop.StateRunning, st)
}
