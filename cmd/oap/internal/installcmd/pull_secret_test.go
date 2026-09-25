package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func deploy(name, image string, args []string) *unstructured.Unstructured {
	c := map[string]any{"name": "c", "image": image}
	if args != nil {
		c["args"] = toAnySlice(args)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{c},
		}}},
	}}
}
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func podImagePullSecrets(t *testing.T, d *unstructured.Unstructured) []string {
	ips, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "imagePullSecrets")
	require.NoError(t, err)
	if !found {
		return nil
	}
	var out []string
	for _, e := range ips {
		out = append(out, e.(map[string]any)["name"].(string))
	}
	return out
}

func TestInjectImagePullSecret(t *testing.T) {
	fp := map[string]bool{"myreg.io/ap/spicebox-operator:dev": true, "myreg.io/ap/agentprimitives-webd:dev": true}

	// first-party operator: gets the secret AND the --image-pull-secret arg
	op := deploy("spicebox-operator", "myreg.io/ap/spicebox-operator:dev", []string{"--runner-image=x"})
	require.NoError(t, injectImagePullSecret(op, "creds", fp, "spicebox-operator"))
	assert.Equal(t, []string{"creds"}, podImagePullSecrets(t, op))
	args, _, _ := unstructured.NestedStringSlice(op.Object, "spec", "template", "spec", "containers")
	_ = args
	opArgs, _, _ := unstructured.NestedSlice(op.Object, "spec", "template", "spec", "containers")
	gotArgs := opArgs[0].(map[string]any)["args"].([]any)
	assert.Contains(t, anyToStrings(gotArgs), "--image-pull-secret=creds")

	// first-party webd: gets the secret, NO operator arg
	wd := deploy("agentprimitives-webd", "myreg.io/ap/agentprimitives-webd:dev", nil)
	require.NoError(t, injectImagePullSecret(wd, "creds", fp, "spicebox-operator"))
	assert.Equal(t, []string{"creds"}, podImagePullSecrets(t, wd))

	// public image: untouched
	pub := deploy("spicebox-graphiti", "zepai/graphiti:latest", nil)
	require.NoError(t, injectImagePullSecret(pub, "creds", fp, "spicebox-operator"))
	assert.Nil(t, podImagePullSecrets(t, pub))

	// empty secret: no-op
	op2 := deploy("spicebox-operator", "myreg.io/ap/spicebox-operator:dev", nil)
	require.NoError(t, injectImagePullSecret(op2, "", fp, "spicebox-operator"))
	assert.Nil(t, podImagePullSecrets(t, op2))
}

func anyToStrings(a []any) []string {
	out := make([]string, len(a))
	for i, v := range a {
		out[i], _ = v.(string)
	}
	return out
}
