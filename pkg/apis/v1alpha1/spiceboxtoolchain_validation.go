package v1alpha1

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// reservedToolchainEnvKeys are env names the pod builder computes itself. A
// toolchain that set them would silently shadow the composed value (PATH is
// assembled from every toolchain's Bin; TMPDIR must stay off the 50Mi tmpfs).
var reservedToolchainEnvKeys = map[string]struct{}{
	"PATH":   {},
	"TMPDIR": {},
}

// ToolchainRootFor returns the absolute mount root for a toolchain by name.
func ToolchainRootFor(name string) string { return ToolchainRootPath + "/" + name }

// ValidateToolchainSpec checks a SpiceboxToolchain spec in isolation: no CR
// reads, no registry lookups (the delivery Kind validates its own fields
// separately). Returns one error listing every problem, or nil when clean.
func ValidateToolchainSpec(name string, s SpiceboxToolchainSpec) error {
	var problems []string

	// name flows into two places a cluster-scoped CR's more permissive
	// metadata.name (a DNS-1123 subdomain — dots and up to 253 chars allowed)
	// does not satisfy: it becomes a Kubernetes container name
	// ("toolchain-"+name, a corev1.Container.Name, which must be a DNS-1123
	// label — no dots, max 63 chars) and a pod filesystem path segment via
	// ToolchainRootFor(name). Catch a name like "go.1.21" here, at admission,
	// rather than letting it fail much later when the Pod is submitted.
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		problems = append(problems, fmt.Sprintf(
			"name %q is not a DNS-1123 label: %s", name, strings.Join(errs, "; ")))
	}
	if containerName := "toolchain-" + name; len(containerName) > validation.DNS1123LabelMaxLength {
		problems = append(problems, fmt.Sprintf(
			"name %q is too long: %q (name prefixed with \"toolchain-\") exceeds the DNS-1123 label max of %d characters",
			name, containerName, validation.DNS1123LabelMaxLength))
	}

	if s.Source.Kind == "" {
		problems = append(problems, "source.kind is required")
	}
	if s.Source.Image == "" {
		problems = append(problems, "source.image is required")
	}
	if want := ToolchainRootFor(name); s.Source.Prefix != want {
		problems = append(problems, fmt.Sprintf(
			"source.prefix %q must equal %q (build path must equal mount path)",
			s.Source.Prefix, want))
	}
	for i, b := range s.Bin {
		switch {
		case b == "":
			problems = append(problems, fmt.Sprintf("bin[%d] is empty", i))
		case strings.HasPrefix(b, "/"):
			problems = append(problems, fmt.Sprintf("bin[%d] %q must be relative to the toolchain root", i, b))
		case strings.Contains(b, ".."):
			problems = append(problems, fmt.Sprintf("bin[%d] %q must not contain %q", i, b, ".."))
		case strings.Contains(b, ":"):
			// ':' is the PATH separator; an embedded one silently splits this single
			// bin entry into two PATH segments (e.g. "bin:/etc" appends "/etc").
			problems = append(problems, fmt.Sprintf("bin[%d] %q must not contain %q (the PATH separator)", i, b, ":"))
		}
	}
	if s.SizeBytes <= 0 {
		problems = append(problems, "sizeBytes must be > 0 (the operator sizes ephemeral storage from it)")
	}
	for k := range s.Env {
		if _, bad := reservedToolchainEnvKeys[k]; bad {
			problems = append(problems, fmt.Sprintf("env[%q] is reserved and computed by the pod builder", k))
		}
	}
	// Expansion doubles as template validation: anything left holding "{{"
	// after substitution names a variable we do not supply.
	if _, err := ExpandToolchainEnv(s.Env, ToolchainRootFor(name), ToolchainCachePath); err != nil {
		problems = append(problems, err.Error())
	}

	if len(problems) == 0 {
		return nil
	}
	// Sorted so the resulting status-condition message is stable across
	// reconciles (map iteration order is not).
	sort.Strings(problems)
	return fmt.Errorf("toolchain %q invalid: %s", name, strings.Join(problems, "; "))
}

// ExpandToolchainEnv substitutes the two supported template variables. It is a
// literal replace, not text/template, so the supported spelling is exactly
// "{{ .Root }}" and "{{ .Cache }}". Any residual "{{" fails closed rather than
// reaching a pod as a literal brace — a silently-unexpanded GOCACHE would send
// the build cache to a path named "{{.Cache}}/gobuild".
func ExpandToolchainEnv(env map[string]string, root, cache string) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	r := strings.NewReplacer("{{ .Root }}", root, "{{ .Cache }}", cache)
	out := make(map[string]string, len(env))
	var bad []string
	for k, v := range env {
		ev := r.Replace(v)
		if strings.Contains(ev, "{{") {
			bad = append(bad, fmt.Sprintf("env[%q]=%q has an unexpanded template (only %q and %q are supported)",
				k, v, "{{ .Root }}", "{{ .Cache }}"))
			continue
		}
		out[k] = ev
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return out, nil
}
