package settingseditor

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// SpecFromYAML parses the raw editor's YAML into a SettingsSpec. It rejects
// multi-document input explicitly — sigs.k8s.io/yaml reads only the first
// document, and silent truncation here would apply a different spec than the
// one on screen — and rejects unknown fields so typos fail loudly instead of
// vanishing at apply time.
func SpecFromYAML(b []byte) (*v1alpha1.SettingsSpec, error) {
	if err := rejectMultiDocument(b); err != nil {
		return nil, err
	}
	var spec v1alpha1.SettingsSpec
	if err := yaml.UnmarshalStrict(b, &spec); err != nil {
		return nil, fmt.Errorf("parse settings YAML: %w", err)
	}
	return &spec, nil
}

// SpecToYAML renders the spec for the raw editor.
func SpecToYAML(spec *v1alpha1.SettingsSpec) ([]byte, error) {
	b, err := yaml.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("render settings YAML: %w", err)
	}
	return b, nil
}

// rejectMultiDocument errors when a second YAML document with any non-comment
// content follows a "---" separator. A "---" preceded only by blank lines and
// comments is the FIRST document's opening marker, not a second document —
// but a "---" after another "---" means the first document was empty and the
// content after it is a second doc sigs.k8s.io/yaml would silently drop.
func rejectMultiDocument(b []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	inSecondDoc := false
	contentSeen := false
	separatorSeen := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			if contentSeen || separatorSeen {
				inSecondDoc = true
			}
			separatorSeen = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if inSecondDoc {
			return fmt.Errorf("multi-document YAML is not supported here: only the first document would be applied")
		}
		contentSeen = true
	}
	return sc.Err()
}
