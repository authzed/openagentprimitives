// pkg/controllers/relationshipsource/tenantinput_test.go
//
// Hostile TENANT-WRITABLE input, driven through the real Reconcile, asserted at
// BOTH sinks.
//
// This file exists because of how the previous scrubbing bug got through. The
// analysis behind it was careful and it was wrong in one specific way: it
// reasoned about the strings the KINDS construct — `github: GET %s: ...`, whose
// %s is always a URL the kind built with url.URL.String() — and concluded that
// several shapes were unreachable. The shape that actually leaked was one an
// OPERATOR typed into spec.baseURL, which is not required to be a URL at all
// and which credhost quotes back verbatim. Reasoning about the producer missed
// the producer that matters.
//
// So the direction here is the other one: start from the fields a tenant can
// write, push the nastiest thing that fits in each, and assert on what comes
// out. BOTH sinks, every case — the Ready condition message AND every
// MonitoringEvent.Summary — because the last bug was present in both and
// checking either one alone would have called it fixed.
//
// Where a shape is genuinely not redacted, this file ASSERTS that rather than
// arguing it in a comment. An asserted limitation is a fact that breaks loudly
// if it changes. A reasoned one is what failed last time.
package relationshipsource

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// secret is the value every case below tries to smuggle out. One literal, so a
// case that leaks names itself in the failure message.
const tenantSecret = "notarealtoken-zz9"

// sink is one place operator-visible text lands. Named so a failure says WHICH
// audience saw the value — status is readable by anything with `get
// relationshipsources`, a Summary goes to a chat channel.
type sink struct {
	name string
	text string
}

// reconcileSinks runs one reconcile and returns everything it surfaced to an
// operator: the Ready condition's message, and every MonitoringEvent.Summary
// published. Returning them together is the point — a fix that moved a leak
// from one to the other would satisfy neither assertion.
func reconcileSinks(t *testing.T, r *Reconciler, c client.Client, pub *publishCapture, key types.NamespacedName) []sink {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err, "every failure here is condition-surfaced, never returned")

	var out []sink
	var got spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(), key, &got))
	if cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady); cond != nil {
		out = append(out, sink{name: "Ready condition message", text: cond.Message})
	}
	if lp := got.Status.Sync.LastPass; lp != nil {
		for i, s := range lp.ScopeErrorSamples {
			out = append(out, sink{name: "scope error sample " + string(rune('0'+i)), text: s.Scope + " " + s.Message})
		}
	}
	for _, ev := range pub.snapshot() {
		out = append(out, sink{name: "MonitoringEvent.Summary (" + ev.Reason + ")", text: ev.Summary})
	}
	require.NotEmpty(t, out,
		"a case that surfaces nothing proves nothing: it would pass with the scrubber deleted")
	return out
}

// tenantCase is one hostile input and what must (and must not) come back out.
type tenantCase struct {
	name string
	// baseURL, kind and config are the tenant-writable spec fields. kind
	// defaults to the registered fixture kind when empty.
	baseURL string
	kind    string
	config  string
	// listErrQuotesConfig makes the registered kind fail enumeration quoting
	// spec.config back — what a kind does when it cannot parse its own
	// configuration, and the only route by which spec.config reaches a sink.
	listErrQuotesConfig bool
	// allowedHosts on the credential. Non-empty is required to reach
	// credhost.Check at all; an unscoped credential is refused earlier, with a
	// message that never quotes the destination.
	allowedHosts []string
	// wantAbsent must appear in NO sink. wantPresentSomewhere must appear in at
	// least one, so a case cannot pass by redacting the entire message.
	wantAbsent           []string
	wantPresentSomewhere []string
}

func TestReconcile_HostileTenantInputReachesNeitherSink(t *testing.T) {
	cases := []tenantCase{
		{
			// The shape that actually broke: spec.baseURL is not required to
			// be a well-formed URL, and credhost's "names no host" branch
			// quotes whatever was typed, in full.
			name:                 "a non-URL destination's query is redacted",
			baseURL:              "/Groups?access_token=" + tenantSecret,
			allowedHosts:         []string{"directory.example.internal"},
			wantAbsent:           []string{tenantSecret, "access_token"},
			wantPresentSomewhere: []string{"/Groups?<redacted>", "names no host"},
		},
		{
			// The OAuth implicit flow returns its token in the fragment.
			name:                 "a fragment-borne token is redacted",
			baseURL:              "/cb#access_token=" + tenantSecret,
			allowedHosts:         []string{"directory.example.internal"},
			wantAbsent:           []string{tenantSecret, "access_token"},
			wantPresentSomewhere: []string{"/cb#<redacted>"},
		},
		{
			// Previously argued unreachable because url.URL.String() lowercases
			// a scheme. True of what a KIND builds; false of what an operator
			// types, and the unanchored pass is what now covers it — the
			// anchored one still requires lowercase.
			name:                 "an UPPERCASE scheme does not evade the query redaction",
			baseURL:              "HTTPS:/x?token=" + tenantSecret,
			allowedHosts:         []string{"directory.example.internal"},
			wantAbsent:           []string{tenantSecret, "token="},
			wantPresentSomewhere: []string{"?<redacted>"},
		},
		{
			name:                 "userinfo credentials are redacted",
			baseURL:              "https://svc:" + tenantSecret + "@",
			allowedHosts:         []string{"directory.example.internal"},
			wantAbsent:           []string{tenantSecret},
			wantPresentSomewhere: []string{"<redacted>@"},
		},
		{
			// GOOD NEWS, asserted so it stays true: when the destination IS a
			// well-formed URL, credhost's refusal quotes only the HOST. The
			// path never reaches a sink at all, so a token hidden in a path
			// segment is not exposed by this route — no scrubbing required,
			// because nothing quotes it.
			name:                 "a path-segment credential never reaches a sink on a well-formed URL",
			baseURL:              "https://elsewhere.example/scim/v2/token/" + tenantSecret + "/Groups",
			allowedHosts:         []string{"directory.example.internal"},
			wantAbsent:           []string{tenantSecret, "/scim/v2/"},
			wantPresentSomewhere: []string{"elsewhere.example", "directory.example.internal"},
		},
		{
			// spec.kind is free text: MinLength=1, no pattern, no enum. It
			// reaches the Ready condition on the unregistered path, so it is
			// scrubbed like every other tenant-writable value.
			name:                 "a query smuggled through spec.kind is redacted",
			kind:                 "notakind?token=" + tenantSecret,
			wantAbsent:           []string{tenantSecret, "token="},
			wantPresentSomewhere: []string{"<redacted>", "not a registered relsync kind"},
		},
		{
			// The only route by which spec.config reaches a sink: a kind that
			// cannot parse its own configuration and quotes it back. The error
			// lands in ScopeErrors, and from there in the enumeration-failed
			// condition message.
			name:                 "config quoted back by a failing kind is redacted",
			config:               `{"endpoint":"https://h/x?api_key=` + tenantSecret + `"}`,
			listErrQuotesConfig:  true,
			wantAbsent:           []string{tenantSecret, "api_key"},
			wantPresentSomewhere: []string{"<redacted>", "nothing was synced this pass"},
		},
		{
			// A KNOWN LIMITATION, asserted rather than argued. A query
			// separated from its "?" by a literal space is not redacted: the
			// pattern requires the parameter list to be one unbroken run, and
			// loosening it to "any key=value anywhere" would eat the diagnostic
			// substance of nearly every message here (status=403, reason=...).
			//
			// Accepted because the exploit requires an operator to type a
			// space into their own destination, which breaks their own sync,
			// and because the alternative costs every message's readability.
			// If this assertion ever fails, the scrubber got broader — check
			// that the trade was made deliberately.
			name:                 "KNOWN LIMIT: a space-separated query is NOT redacted",
			baseURL:              "/x? token=" + tenantSecret,
			allowedHosts:         []string{"directory.example.internal"},
			wantPresentSomewhere: []string{tenantSecret},
		},
		{
			// THE OVER-REDACTION TRADE, pinned so the next reader sees it was
			// chosen. Any "?"-run containing "=" goes, diagnostic or not, so a
			// message whose useful content sits in a query loses it. Text that
			// reaches a chat channel errs toward redaction; this is the cost.
			name:                 "CHOSEN TRADE: a harmless diagnostic query is redacted too",
			baseURL:              "/x?status=403&reason=forbidden",
			allowedHosts:         []string{"directory.example.internal"},
			wantAbsent:           []string{"status=403", "reason=forbidden"},
			wantPresentSomewhere: []string{"?<redacted>", "names no host"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kindName := "fakekind-tenant-" + strings.Map(func(r rune) rune {
				if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
					return r
				}
				return '-'
			}, tc.name)

			src := newSrc("ns", "src", kindName)
			src.Spec.BaseURL = tc.baseURL
			if tc.kind != "" {
				src.Spec.Kind = tc.kind
			}
			if tc.config != "" {
				src.Spec.Config = &apiextensionsv1.JSON{Raw: []byte(tc.config)}
			}
			id, sec := authFixtures("ns", tc.allowedHosts...)
			c := newClient(t, src, id, sec)

			fk := &fakeKind{
				name:   kindName,
				source: relsource.Source{Name: "fakekindtenantsync"},
				pages:  []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "fake_scope"}}, Complete: true}},
			}
			if tc.listErrQuotesConfig {
				// Exactly what a kind does with configuration it cannot use.
				fk.listErr = errors.New("fakekind: cannot parse config " + tc.config)
			}
			relsync.Register(fk)

			sdb := &fakeSpiceDB{writer: &fakeRelWriter{}, reader: &fakeReader{}}
			pub := &publishCapture{}
			r := &Reconciler{Client: c, SecretReader: newSecretReader(c), SpiceDB: sdb, MonitoringPublish: pub.publish}

			sinks := reconcileSinks(t, r, c, pub, types.NamespacedName{Namespace: "ns", Name: "src"})

			for _, s := range sinks {
				for _, bad := range tc.wantAbsent {
					assert.NotContains(t, s.text, bad,
						"%s leaked %q — this sink is read by an audience wider than the CR", s.name, bad)
				}
			}
			joined := ""
			for _, s := range sinks {
				joined += s.text + "\n"
			}
			for _, want := range tc.wantPresentSomewhere {
				assert.Contains(t, joined, want,
					"redaction must not cost the diagnosis; sinks were:\n%s", joined)
			}
		})
	}
}
