package crddocs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.demo.example
spec:
  group: demo.example
  names:
    kind: Widget
    plural: widgets
    singular: widget
    shortNames: [wg]
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          description: Widget is a demo resource.
          type: object
          properties:
            spec:
              type: object
              required: [size]
              properties:
                size:
                  type: string
                  description: The size, with <brackets> and {braces}.
                  enum: [small, large]
                  default: small
                tags:
                  type: array
                  items:
                    type: object
                    properties:
                      key:
                        type: string
                        description: A tag key.
            status:
              type: object
              properties:
                phase:
                  type: string
                  description: The phase.
`

func TestGenerate(t *testing.T) {
	crdDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(crdDir, "widget.yaml"), []byte(sampleCRD), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(crdDir, "kustomization.yaml"), []byte("resources: []\n"), 0o644))

	out := t.TempDir()
	require.NoError(t, Generate(crdDir, out))

	overB, err := os.ReadFile(filepath.Join(out, "crd-reference.mdx"))
	require.NoError(t, err)
	over := string(overB)
	assert.Contains(t, over, "group: 'CRD reference'")
	assert.Contains(t, over, "[Widget](#/crd-widget)", "kind listed in the overview")

	pageB, err := os.ReadFile(filepath.Join(out, "crd-widget.mdx"))
	require.NoError(t, err)
	s := string(pageB)
	assert.Contains(t, s, "title: 'Widget'")
	assert.Contains(t, s, "Short names** `wg`")
	assert.Contains(t, s, "`spec.size`", "spec field with dotted path")
	assert.Contains(t, s, `\*`, "required field marked")
	assert.Contains(t, s, `enum: small \| large`, "enum rendered (cell separator escaped)")
	assert.Contains(t, s, "default: small")
	assert.Contains(t, s, "&lt;brackets&gt;", "description is MDX-escaped")
	assert.Contains(t, s, "`spec.tags[].key`", "array-of-object nested path")
	assert.Contains(t, s, "## Status")
	assert.Contains(t, s, "`status.phase`")

	_, statErr := os.Stat(filepath.Join(out, "crd-kustomization.mdx"))
	assert.True(t, os.IsNotExist(statErr), "kustomization.yaml is not a CRD")
}
