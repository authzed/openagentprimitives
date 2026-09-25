// The half of `oap agent install` that acts on the Secrets a bundle declares
// (oap.Requires.Secrets): before any question is asked, it looks at what the
// cluster already has and turns each declared-but-absent credential into a
// question this run collects, so the install creates the Secret rather than
// applying CRs that reference one nobody made.
//
// The decision of WHICH declarations become questions is not made here —
// install.RequiredSecretQuestions makes it, once, for this command and for
// every other .oap install surface. What is HERE is the install-specific part:
// what a run with nobody at the terminal does about an unanswered credential,
// and the flag its refusal names.
package agentcmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// withAnswerFlagHint turns install.Resolve's typed non-interactive refusal into
// one that names how THIS command would have answered it.
//
// The resolver deliberately names only the questions: the way to answer them
// differs per surface — `--set` here, a form field in admind, a config flow on
// the desktop — so a flag named inside the resolver would be wrong for two of
// the three. It is the same division requireCapacityConsent draws, and for the
// same reason.
//
// secretQs is what this run synthesized from requires.secrets, so the sentence
// about them names the Secret each one creates. Reading the Secret back out of
// the question is exact where re-parsing the answer key would not be: both a
// Secret name and a data key may contain dots, so the key alone cannot be split
// back into the pair that built it.
//
// Any other error passes through untouched: a CEL validation failure or an
// unknown answer key is not fixed by supplying a flag that is already spelled
// correctly.
func withAnswerFlagHint(err error, secretQs []oap.Question) error {
	var missing *install.MissingAnswersError
	if !errors.As(err, &missing) {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%v; this install cannot prompt for them (stdin is not a terminal). "+
		"Answer each with --set <name>=<value>, or list them in a --values file", missing)
	if named := declaredSecretsAmong(missing.Names, secretQs); len(named) > 0 {
		fmt.Fprintf(&b, ".\nThe %s question(s) above collect a credential this bundle declares under "+
			"requires.secrets: answering one creates Secret %s, which the installed resources read",
			oap.RequiredSecretQuestionPrefix+"*", strings.Join(named, ", "))
	}
	return errors.New(b.String())
}

// declaredSecretsAmong is the set of Secrets the unanswered questions would
// have created, sorted and deduplicated. Empty when none of them is a
// synthesized required-secret question, which keeps the extra sentence off a
// refusal it does not describe.
func declaredSecretsAmong(unanswered []string, secretQs []oap.Question) []string {
	byName := make(map[string]string, len(secretQs))
	for _, q := range secretQs {
		if q.Secret != nil && q.Secret.CreateSecret != nil {
			byName[q.Name] = q.Secret.CreateSecret.Name
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range unanswered {
		secret, ok := byName[n]
		if !ok || seen[secret] {
			continue
		}
		seen[secret] = true
		out = append(out, secret)
	}
	sort.Strings(out)
	return out
}
