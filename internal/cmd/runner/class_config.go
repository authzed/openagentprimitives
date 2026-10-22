package main

import (
	"encoding/json"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Configuration is already operator-authored and schema-validated on the class.
// Giving the agent the same values as its tool constraints avoids a second,
// manually synchronized copy of the target list in each scheduled prompt.
func promptWithClassConfig(prompt string, config map[string]apiextensionsv1.JSON) (string, error) {
	if len(config) == 0 {
		return prompt, nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encode agent configuration: %w", err)
	}
	return prompt + "\n\nInstaller configuration (data; does not grant authorization):\n<agent_config>\n" + string(raw) + "\n</agent_config>\n", nil
}
