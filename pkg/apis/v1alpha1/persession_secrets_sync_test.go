package v1alpha1_test

// This file deliberately lives in the v1alpha1_test (external) package, not
// v1alpha1, so it can import pkg/controllers/agentsession and
// pkg/web/secretoutsrv — both of which import v1alpha1 — without creating an
// import cycle. It pins that MemoryTokenSecretName and SecretOutputSecretName
// build their Secret name from the suffix consts declared in
// pkg/apis/v1alpha1/persession_secrets.go, so PerSessionSecretSuffixes (and
// therefore ToolCall.ValidateCredentialSourceOwnership) never drifts out of
// sync with the literal each owning package actually uses.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
)

func TestPerSessionSecretSuffixesMatchOwners(t *testing.T) {
	const sessionName = "session-fake"

	sess := &spiceboxv1alpha1.AgentSession{}
	sess.Name = sessionName

	assert.Equal(t,
		sessionName+spiceboxv1alpha1.MemoryTokenSecretSuffix,
		agentsession.MemoryTokenSecretName(sess),
		"agentsession.MemoryTokenSecretName must build from v1alpha1.MemoryTokenSecretSuffix")

	assert.Equal(t,
		sessionName+spiceboxv1alpha1.SecretOutputSecretSuffix,
		secretoutsrv.SecretOutputSecretName(sessionName),
		"secretoutsrv.SecretOutputSecretName must build from v1alpha1.SecretOutputSecretSuffix")

	assert.Equal(t,
		sessionName+spiceboxv1alpha1.PassthroughCredentialSecretSuffix,
		spiceboxv1alpha1.PassthroughCredentialSecretName(sessionName),
		"v1alpha1.PassthroughCredentialSecretName must build from v1alpha1.PassthroughCredentialSecretSuffix")
}
