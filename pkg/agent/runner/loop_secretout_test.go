package runner

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// --- stubs ---

type noopPublisher struct{}

func (noopPublisher) Publish(_ context.Context, _ string, _ []byte, _ string) error { return nil }

type recordingPublisher struct {
	name, handle string
	value        []byte
	called       bool
}

func (p *recordingPublisher) Publish(_ context.Context, name string, value []byte, handle string) error {
	p.called = true
	p.name = name
	p.value = value
	p.handle = handle
	return nil
}

type failingPublisher struct{ err error }

func (f *failingPublisher) Publish(_ context.Context, _ string, _ []byte, _ string) error {
	return f.err
}

type recordingStatusWriter struct {
	name, handle, secretName string
	called                   bool
}

func (s *recordingStatusWriter) WriteSatisfiedSecretOutput(_ context.Context, name, handle, secretName string) error {
	s.called = true
	s.name = name
	s.handle = handle
	s.secretName = secretName
	return nil
}

type secretOutFailingStore struct{}

func (secretOutFailingStore) Put(context.Context, string, []byte) (string, error) {
	return "", secretOutPutErr
}
func (secretOutFailingStore) Get(string) (secretout.Entry, bool) { return secretout.Entry{}, false }

var secretOutPutErr = &secretOutStoreErr{}

type secretOutStoreErr struct{}

func (*secretOutStoreErr) Error() string { return "boom" }

// --- tests ---

func TestApplySecretOutput_DivertsValueReplacesContentWithHandle(t *testing.T) {
	store := secretout.NewSessionStore("s1")
	in := tool.Result{
		Content:      "apiVersion: v1\nclusters: [secret-kubeconfig]",
		SecretOutput: &secretout.Spec{Name: "kubeconfig", Description: "cluster admin; expires 1h"},
	}
	out := applySecretOutput(context.Background(), store, noopPublisher{}, nil, "sess", in, "tooluse-1")

	assert.NotContains(t, out.Content, "secret-kubeconfig", "value not in Content")
	assert.Contains(t, out.Content, "kubeconfig")
	assert.Contains(t, out.Content, "expires 1h")
	assert.Regexp(t, `so-[0-9a-f]{24}`, out.Content)
	assert.Nil(t, out.SecretOutput, "cleared so downstream never re-diverts")

	handle := regexp.MustCompile(`so-[0-9a-f]{24}`).FindString(out.Content)
	e, ok := store.Get(handle)
	require.True(t, ok)
	assert.Equal(t, []byte("apiVersion: v1\nclusters: [secret-kubeconfig]"), e.Value)
}

func TestApplySecretOutput_StoreFailureSurfacesErrorNotValue(t *testing.T) {
	out := applySecretOutput(context.Background(), secretOutFailingStore{}, noopPublisher{}, nil, "sess", tool.Result{
		Content:      "the-secret",
		SecretOutput: &secretout.Spec{Name: "k", Description: "d"},
	}, "tu")
	assert.True(t, out.IsError)
	assert.NotContains(t, out.Content, "the-secret", "never leak the value on failure")
}

func TestApplySecretOutput_NilSpecPassThrough(t *testing.T) {
	out := applySecretOutput(context.Background(), secretout.NewSessionStore("s"), noopPublisher{}, nil, "sess", tool.Result{Content: "normal"}, "tu")
	assert.Equal(t, "normal", out.Content)
}

func TestApplySecretOutput_PublishSuccess_PublisherAndStatusWriterCalled(t *testing.T) {
	store := secretout.NewSessionStore("s1")
	pub := &recordingPublisher{}
	sw := &recordingStatusWriter{}

	in := tool.Result{
		Content:      "super-secret-value",
		SecretOutput: &secretout.Spec{Name: "apikey", Description: "API key for service X"},
	}
	out := applySecretOutput(context.Background(), store, pub, sw, "my-session", in, "tooluse-2")

	// Content has handle, not value.
	assert.False(t, out.IsError)
	assert.NotContains(t, out.Content, "super-secret-value", "value must not appear in Content on success")
	assert.Contains(t, out.Content, "API key for service X")
	assert.Regexp(t, `so-[0-9a-f]{24}`, out.Content)

	// Publisher received the right args.
	require.True(t, pub.called, "publisher must have been called")
	assert.Equal(t, "apikey", pub.name)
	assert.Equal(t, []byte("super-secret-value"), pub.value)
	assert.Regexp(t, `so-[0-9a-f]{24}`, pub.handle)

	// Handle in Content matches handle sent to publisher.
	handle := regexp.MustCompile(`so-[0-9a-f]{24}`).FindString(out.Content)
	assert.Equal(t, handle, pub.handle, "handle in Content must match handle sent to publisher")

	// Status writer recorded the handle and the secret-output name (used by
	// the sandbox tool's write-once fast-fail pre-check).
	require.True(t, sw.called, "status writer must have been called")
	assert.Equal(t, "apikey", sw.name)
	assert.Equal(t, pub.handle, sw.handle)
	assert.Equal(t, "my-session-secret-outputs", sw.secretName)
}

func TestApplySecretOutput_PublishFailure_ErrorResultNoValueLeak(t *testing.T) {
	store := secretout.NewSessionStore("s1")
	pubErr := errors.New("operator unavailable")
	pub := &failingPublisher{err: pubErr}
	sw := &recordingStatusWriter{}

	in := tool.Result{
		Content:      "the-secret-value",
		SecretOutput: &secretout.Spec{Name: "cred", Description: "credentials for system"},
	}
	out := applySecretOutput(context.Background(), store, pub, sw, "my-session", in, "tooluse-3")

	assert.True(t, out.IsError, "publish failure must produce an error Result")
	assert.NotContains(t, out.Content, "the-secret-value", "value must never appear in Content on publish failure")
	assert.Contains(t, out.Content, "cred", "error message should name the secret")
	assert.False(t, sw.called, "status writer must NOT be called when publish fails")
}
