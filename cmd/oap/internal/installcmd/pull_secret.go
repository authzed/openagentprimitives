package installcmd

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// injectImagePullSecret adds imagePullSecrets:[{secret}] to a Deployment's pod
// template when (a) secret is non-empty and (b) a container image is first-party
// (a value in firstPartyRefs). For the operator Deployment (by metadata.name ==
// operatorName) it also appends --image-pull-secret=<secret> to the container
// args so the operator stamps the secret onto runner/detector pods. No-op for
// non-Deployments, public-image Deployments, or an empty secret.
func injectImagePullSecret(doc *unstructured.Unstructured, secret string, firstPartyRefs map[string]bool, operatorName string) error {
	if secret == "" || doc.GetKind() != "Deployment" {
		return nil
	}
	containers, found, err := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return err
	}
	firstParty := false
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if img, _ := cm["image"].(string); firstPartyRefs[img] {
			firstParty = true
		}
	}
	if !firstParty {
		return nil
	}

	ips, _, err := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", "imagePullSecrets")
	if err != nil {
		return err
	}
	ips = append(ips, map[string]any{"name": secret})
	if err := unstructured.SetNestedSlice(doc.Object, ips, "spec", "template", "spec", "imagePullSecrets"); err != nil {
		return err
	}

	if doc.GetName() == operatorName {
		// append --image-pull-secret=<secret> to the operator container's args
		c0, _ := containers[0].(map[string]any)
		args, _ := c0["args"].([]any)
		args = append(args, "--image-pull-secret="+secret)
		c0["args"] = args
		containers[0] = c0
		if err := unstructured.SetNestedSlice(doc.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return err
		}
	}
	return nil
}
