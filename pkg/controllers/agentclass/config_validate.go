package agentclass

import (
	"encoding/json"
	"fmt"
	"regexp"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// configRefRe matches a `config.<ident>` reference in a toolspec constraint's
// CEL. Used to cross-check that every config key a bound toolspec reads is
// declared in the class's configSchema (so a typo fails at apply, not as a
// runtime deny). Deliberately simple: the identifier grammar CEL allows for a
// member of the `config` map root.
var configRefRe = regexp.MustCompile(`\bconfig\.([A-Za-z_][A-Za-z0-9_]*)`)

// configRefsFromToolspecs returns the distinct config keys referenced as
// `config.<ident>` across every bound toolspec's constraints.
func configRefsFromToolspecs(specs []v1alpha1.SpiceboxToolspec) []string {
	seen := map[string]struct{}{}
	var out []string
	for i := range specs {
		for _, c := range specs[i].Spec.Constraints {
			for _, m := range configRefRe.FindAllStringSubmatch(c.CEL, -1) {
				if _, ok := seen[m[1]]; !ok {
					seen[m[1]] = struct{}{}
					out = append(out, m[1])
				}
			}
		}
	}
	return out
}

// validateConfig checks spec.config against spec.configSchema and verifies that
// every config key referenced by a bound toolspec constraint (celConfigRefs) is
// declared. It returns a non-nil error naming the offending key and rule; the
// caller sets Valid=False with that message (fail-closed).
func validateConfig(spec *v1alpha1.AgentClassSpec, celConfigRefs []string) error {
	byName := make(map[string]v1alpha1.ConfigKeySchema, len(spec.ConfigSchema))
	for _, k := range spec.ConfigSchema {
		byName[k.Name] = k
	}

	// Every declared key: required-presence + type + pattern/enum.
	for _, k := range spec.ConfigSchema {
		raw, present := spec.Config[k.Name]
		if !present {
			if k.Required {
				return fmt.Errorf("config key %q is required by configSchema but absent from spec.config", k.Name)
			}
			continue
		}
		if err := validateConfigValue(k, raw); err != nil {
			return fmt.Errorf("config key %q: %w", k.Name, err)
		}
	}

	// Every config key present must be declared (no undeclared config).
	for name := range spec.Config {
		if _, ok := byName[name]; !ok {
			return fmt.Errorf("config key %q is set in spec.config but not declared in configSchema", name)
		}
	}

	// Every config.X a bound toolspec constraint references must be declared.
	for _, ref := range celConfigRefs {
		if _, ok := byName[ref]; !ok {
			return fmt.Errorf("a bound toolspec constraint references config.%s, which is not declared in configSchema", ref)
		}
	}
	return nil
}

func validateConfigValue(k v1alpha1.ConfigKeySchema, raw apiextensionsv1.JSON) error {
	switch k.Type {
	case "string", "enum":
		var s string
		if err := json.Unmarshal(raw.Raw, &s); err != nil {
			return fmt.Errorf("must be a string")
		}
		return validateConfigString(k, s)
	case "stringList":
		var xs []string
		if err := json.Unmarshal(raw.Raw, &xs); err != nil {
			return fmt.Errorf("must be a stringList (JSON array of strings)")
		}
		for _, s := range xs {
			if err := validateConfigString(k, s); err != nil {
				return fmt.Errorf("item %q: %w", s, err)
			}
		}
		return nil
	case "int":
		var n int64
		if err := json.Unmarshal(raw.Raw, &n); err != nil {
			return fmt.Errorf("must be an int")
		}
		return nil
	case "bool":
		var b bool
		if err := json.Unmarshal(raw.Raw, &b); err != nil {
			return fmt.Errorf("must be a bool")
		}
		return nil
	default:
		return fmt.Errorf("unknown configSchema type %q", k.Type)
	}
}

func validateConfigString(k v1alpha1.ConfigKeySchema, s string) error {
	if len(k.Enum) > 0 {
		ok := false
		for _, e := range k.Enum {
			if e == s {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("value %q is not one of the enum %v", s, k.Enum)
		}
	}
	if k.Pattern != "" {
		re, err := regexp.Compile(k.Pattern)
		if err != nil {
			return fmt.Errorf("configSchema pattern %q is invalid: %w", k.Pattern, err)
		}
		if !re.MatchString(s) {
			return fmt.Errorf("value %q does not match pattern %q", s, k.Pattern)
		}
	}
	return nil
}
