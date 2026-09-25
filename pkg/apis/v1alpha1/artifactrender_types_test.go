package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestArtifactRender_DecodeYAML_Inline(t *testing.T) {
	src := []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: ArtifactRender
metadata:
  name: ar-pmsess-7-abc123
  namespace: default
spec:
  kind: html
  filename: report.html
  altText: "Engineering vs leadership goals — week of 2026-05-04."
  payload: PGgxPmhlbGxvPC9oMT4=
  timeoutSeconds: 30
`)
	var out spiceboxv1alpha1.ArtifactRender
	require.NoError(t, yaml.Unmarshal(src, &out), "unmarshal ArtifactRender YAML")

	assert.Equal(t, "html", out.Spec.Kind, "Spec.Kind")
	assert.Equal(t, "report.html", out.Spec.Filename, "Spec.Filename")
	assert.Empty(t, out.Spec.PayloadRef, "Spec.PayloadRef should be empty when inline payload provided")
	assert.NotEmpty(t, out.Spec.Payload, "Spec.Payload should be populated from inline base64")
	assert.Equal(t, int32(30), out.Spec.TimeoutSeconds, "Spec.TimeoutSeconds")
}

func TestArtifactRender_DecodeYAML_PayloadRef(t *testing.T) {
	src := []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: ArtifactRender
metadata:
  name: ar-pmsess-7-def456
  namespace: default
spec:
  kind: html
  payloadRef: mem://default/pmsess/render-7/in
  timeoutSeconds: 30
`)
	var out spiceboxv1alpha1.ArtifactRender
	require.NoError(t, yaml.Unmarshal(src, &out), "unmarshal ArtifactRender YAML")

	assert.Equal(t, "mem://default/pmsess/render-7/in", out.Spec.PayloadRef, "Spec.PayloadRef")
	assert.Empty(t, out.Spec.Payload, "Spec.Payload should be empty when payloadRef is used")
}
