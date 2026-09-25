// pkg/platform/oap/install/requiredsecrets.go
//
// The half of install that turns a bundle's `requires.secrets` DECLARATION
// into questions the install actually asks.
//
// It exists because declaring a requirement used to obligate the bundle author
// to also write a matching type=secret question, and a bundle that skipped
// that step installed cleanly and left the agent permanently broken: every CR
// applied, install reporting success, and the AgentIdentity sitting at
// Valid=False with "credentials[...]: secret missing" because nothing had ever
// asked anyone for the credential. The documented remedy was a `kubectl create
// secret generic` in the bundle's README, run BEFORE installing — and skipping
// it was silent.
//
// A declared requirement now gets its prompt for free, and the Secret is
// created through the SAME path a hand-wired question's answer takes (an
// oap.Question with a createSecret target -> Resolve's SecretSpec ->
// Install's Secret write), so it inherits the instance labels, the --name
// rename, the ownership guard, and the adoption label without a second
// pipeline.
package install

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// RequiredSecretQuestions returns the questions this install must ask to
// satisfy the manifest's requires.secrets declarations, plus a notice per
// declaration nothing here can ask for.
//
// The rules, in the order a declaration is judged:
//
//   - The entry NAMES ITS OWN QUESTION. The bundle wired it; Preflight already
//     checked that question can materialize the Secret. Nothing is synthesized,
//     and the operator is never asked twice for one credential.
//   - The entry declares NO KEYS. It names a Secret as a whole — the fixed
//     multi-key convention a setup flow or a channel wizard mints (an OAuth
//     token set, a GitHub App's credential bundle) — so there is no key to
//     write a typed answer under and no single answer that could reconstruct
//     it. It is never asked for; when it is also absent, its absence is
//     RETURNED AS A NOTICE so the operator learns what is still owed at install
//     time rather than from a Valid=False condition later.
//   - The Secret ALREADY EXISTS in ns. Not asked. This is the `orExisting`
//     case, and it is what keeps a re-install, and an operator who prefers to
//     create credentials out of band, working exactly as before.
//   - Otherwise: one required type=secret question per declared key.
//
// namePrefix is what install.Install would prefix onto the objects it creates
// (`--name <n>` -> "<n>-"), and it is applied to the PROBE only. The question's
// createSecret target keeps the declared name, because Install renames the
// Secrets it creates alongside the CRs that reference them — prefixing here too
// would ask about "n-n-widget-token" and create a Secret nothing reads.
//
// The probe is METADATA-ONLY: "does this exist" is a metadata question, and
// answering it with a typed Get would pull the bytes of a credential the
// operator did not ask this command to handle.
//
// FAIL-CLOSED on a probe that cannot run: an unreadable cluster returns an
// error naming the Secret rather than being read as "present". Guessing that
// way reproduces the exact defect this function exists to remove.
func RequiredSecretQuestions(ctx context.Context, r client.Reader, m *oap.Manifest, ns, namePrefix string) ([]oap.Question, []string, error) {
	return requiredSecretQuestions(ctx, r, m, ns, func(name string) (string, error) {
		return namePrefix + name, nil
	})
}

// requiredSecretQuestionsMapped probes the exact graph-planned physical name
// for each declared Secret. The synthesized question still carries the local
// authored name; Prepare applies the node's resource map when it materializes
// the answer.
func requiredSecretQuestionsMapped(ctx context.Context, r client.Reader, m *oap.Manifest, ns string, names map[string]string) ([]oap.Question, []string, error) {
	return requiredSecretQuestions(ctx, r, m, ns, func(name string) (string, error) {
		physical, ok := names["Secret/"+name]
		if !ok || physical == "" {
			return "", fmt.Errorf("required secrets: no physical mapping for Secret/%s", name)
		}
		return physical, nil
	})
}

func requiredSecretQuestions(ctx context.Context, r client.Reader, m *oap.Manifest, ns string, physicalName func(string) (string, error)) ([]oap.Question, []string, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("required secrets: no manifest")
	}

	var questions []oap.Question
	var notices []string
	for _, rs := range m.Requires.Secrets {
		if rs.Question != "" {
			continue
		}
		if ns == "" {
			// A namespaced Get with no namespace reads whatever the reader
			// defaults to, which is not the namespace Install will create the
			// Secret in — so the answer would be about the wrong scope.
			return nil, nil, fmt.Errorf("requires.secrets[%s]: no target namespace to look for it in; pass --namespace", rs.Name)
		}
		physical, err := physicalName(rs.Name)
		if err != nil {
			return nil, nil, err
		}
		present, err := secretExists(ctx, r, ns, physical)
		if err != nil {
			return nil, nil, fmt.Errorf("requires.secrets[%s]: could not determine whether it already exists: %w", rs.Name, err)
		}
		if present {
			continue
		}
		if len(rs.Keys) == 0 {
			notices = append(notices, keylessSecretNotice(rs, ns, physical))
			continue
		}
		for _, key := range rs.Keys {
			questions = append(questions, requiredSecretQuestion(rs, key))
		}
	}
	if err := rejectDuplicateNames(questions); err != nil {
		return nil, nil, err
	}
	return questions, notices, nil
}

// rejectDuplicateNames refuses a synthesized set in which two questions share a
// name. Both a Secret name and a data key may contain dots, so two different
// (Secret, key) pairs CAN produce one answer key — Resolve's answer map is
// keyed by name, so one credential would silently take the other's value and
// one declared key would be left empty. Refusing names both declarations
// instead.
func rejectDuplicateNames(qs []oap.Question) error {
	seen := make(map[string]string, len(qs))
	for _, q := range qs {
		target := q.Secret.CreateSecret.Name + "/" + q.Secret.CreateSecret.Key
		if prior, dup := seen[q.Name]; dup {
			return fmt.Errorf("requires.secrets: %s and %s both answer to %q; "+
				"rename one of them so each declared key has its own answer key", prior, target, q.Name)
		}
		seen[q.Name] = target
	}
	return nil
}

// secretExists reports whether ns/name is present, reading metadata only.
func secretExists(ctx context.Context, r client.Reader, ns, name string) (bool, error) {
	var meta metav1.PartialObjectMetadata
	meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &meta); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// requiredSecretQuestion is one declared key, as the question that collects it.
//
// The shape is an ordinary hand-written createSecret question — nothing about
// it is special to the synthesizer — which is what lets Resolve turn it into a
// SecretSpec and Install create the Secret with no second code path.
func requiredSecretQuestion(rs oap.RequiredSecret, key string) oap.Question {
	desc := "This agent's resources read it from Secret " + rs.Name + ", key " + key + ". " +
		"Install creates the Secret from what you enter here; it is never written to the bundle."
	if p := strings.TrimSpace(rs.Purpose); p != "" {
		desc = p + "\n\n" + desc
	}
	return oap.Question{
		Name:        oap.RequiredSecretQuestionName(rs.Name, key),
		Type:        oap.QSecret,
		Prompt:      fmt.Sprintf("%s (Secret %q, key %q)", key, rs.Name, key),
		Description: desc,
		// Required (the nil default): the bundle says its resources read this,
		// so an install that proceeds without it is the broken install this
		// whole file exists to prevent.
		Secret: &oap.SecretQuestion{
			// The DECLARED name. Install renames what it creates.
			CreateSecret: &oap.SecretTarget{Name: rs.Name, Key: key},
			// The question is only ever synthesized for a Secret that is
			// absent, so "or an existing one" is already how it got here.
			OrExisting: true,
		},
	}
}

// keylessSecretNotice is what the operator is told about a declaration install
// cannot collect: which Secret is missing, in which namespace, and why nothing
// asked. Rendered as a sentence rather than an error because the install itself
// is legitimate — the credential's own setup flow is what mints this shape.
func keylessSecretNotice(rs oap.RequiredSecret, ns, physicalName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Secret %q is declared as required but is absent from namespace %s. ", physicalName, ns)
	if p := strings.TrimSpace(rs.Purpose); p != "" {
		fmt.Fprintf(&b, "It holds: %s. ", strings.TrimSuffix(p, "."))
	}
	b.WriteString("The declaration names no keys, so it is a whole Secret rather than a value anyone can type, " +
		"and install did not ask for it — whatever mints it (a credential setup flow, a channel wizard, or you) " +
		"must create it before the agent can run.")
	return b.String()
}
