package auditcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register every Kind so KindAppendOnly is authoritative in the CLI
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/auditkey"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/publisherkeys"
)

const (
	// publisherKeysNamespace is the operator namespace holding the
	// publisher-keys ConfigMap of component publisher keys.
	publisherKeysNamespace = "agentprimitives-system"
	// publisherKeysConfigMap / Field mirror the operator's persistence
	// names (internal/cmd/operator/main.go) for the component key set.
	publisherKeysConfigMapName = "publisher-keys"
	publisherKeysConfigMapKey  = "keys"
	// unsignedPublisher is the synthetic group nil-provenance append-only
	// entries are bucketed under. They are a warning, never a hard finding.
	unsignedPublisher = "unsigned"
)

func newAuditVerifyCmd(g *apcmd.Globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "verify <session>",
		Short: "Verify a session's append-only audit chains offline",
		Long: `Verify recomputes each publisher's hash chain and Ed25519 signatures
over the session's append-only memory entries, using the K8s-witnessed audit
public key (AgentSession.status), component publisher keys (the
publisher-keys ConfigMap), and any retired session key an operator-signed
record in the scope attests — the keys of AgentSessions that held this name
before, whose own status went with them. It detects tampered payloads (bad signature),
dropped entries (gap), duplicate/relinked entries (fork), entries signed by
untrusted keys (unknown key), and — for ended sessions whose chain heads
were anchored on status — tail truncation.

Exit status is non-zero when any hard finding is present, and also when a
publisher's recorded chain head cannot be decoded — that head is what makes
tail truncation detectable at all, so an undecodable one leaves the tail
unverified. Unsigned entries (pre-provenance or written by an unsigned
writer) are reported as a warning and do NOT, on their own, fail
verification. Requires administrative access to export the complete audit evidence.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuditVerify(cmd, g, args[0], asJSON)
		},
	}
	apcmd.JSONFlag(cmd, &asJSON, "Emit the per-publisher reports as JSON")
	return cmd
}

func runAuditVerify(cmd *cobra.Command, g *apcmd.Globals, sessionName string, asJSON bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	b, err := g.Bundle()
	if err != nil {
		return err
	}
	conn, err := memclient.ConnectAudit(ctx, b, sessionName)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Trust anchors: the session's own audit key (status) + every
	// component publisher key (publisher-keys ConfigMap).
	var sess spiceboxv1alpha1.AgentSession
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &sess); err != nil {
		return fmt.Errorf("get AgentSession %s/%s: %w", b.Namespace, sessionName, err)
	}
	keys := buildKeySet(ctx, b, &sess, errOut)

	// Administrative export includes platform-only links that a session read
	// would omit. Still filter locally to keep the verifier authoritative.
	res, err := conn.Client.QueryAudit(ctx, conn.Scope)
	if err != nil {
		return fmt.Errorf("query memory: %w", err)
	}
	if err := requireCompleteAuditRead(res); err != nil {
		return err
	}
	var entries []memory.Entry
	for _, e := range res.Entries {
		// A signed entry (non-nil provenance) is always part of a chain.
		// A nil-provenance entry counts only if its kind is append-only —
		// mutable-kind entries carry no provenance and are not audited.
		if e.Provenance != nil || memory.KindAppendOnly(e.Kind) {
			entries = append(entries, e)
		}
	}

	// The scope can hold entries signed by a key this session never had: an
	// AgentSession deleted and re-created under the same name leaves its chain
	// behind, and its own key died with its CR. Trust the retired keys the
	// operator witnessed IN the scope, which is why the entries are read first.
	addWitnessedSessionKeys(keys, provenance.SessionPublisher(b.Namespace, sessionName), entries, errOut)
	verifier := provenance.NewVerifier(keys)

	anchors, malformedAnchors := parseAnchors(sess.Status.AuditChainHeads, errOut)
	reports := buildAuditReport(verifier, entries, anchors)

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return err
		}
	} else {
		renderAuditReports(out, g.Theme(out), sessionName, reports)
	}

	// Non-nil error → oap main exits 1. The report is already printed.
	return auditExitError(reports, malformedAnchors)
}

// auditReadLimit caps the scope read the chain walk is built from.
const auditReadLimit = 100000

// errAuditTruncated signals that the scope read stopped at auditReadLimit, so
// the chain walk would have been over part of the ledger. It is a separate
// sentinel because nothing was found WRONG — what failed is that the question
// could not be asked.
var errAuditTruncated = errors.New("audit verification failed: the session holds more append-only entries than one read can return, so the chains cannot be walked end to end")

// requireCompleteAuditRead refuses to verify a truncated read of the scope.
//
// This is the fail-closed direction, and it runs BEFORE any report is built: a
// chain walk over a partial ledger does not produce a slightly-less-complete
// verdict, it produces a wrong one — the missing tail reads as clean, and every
// gap at the cut reads as a hard finding that is not there. Unlike a listing,
// the reader has no flag to raise, so the only honest answer is to say the
// verification could not be performed.
func requireCompleteAuditRead(res memory.QueryResult) error {
	if res.Partial || len(res.DroppedPredicates) != 0 {
		return fmt.Errorf("audit verification failed: incomplete audit export")
	}
	if !res.Truncated {
		return nil
	}
	return fmt.Errorf("%w (limit %d)", errAuditTruncated, auditReadLimit)
}

// errAuditFailed signals a hard finding so the CLI exits non-zero. It is
// printed by main; keep it terse since the report carries the detail.
var errAuditFailed = errors.New("audit verification failed: hard findings present (see report above)")

// errAuditAnchorMalformed signals that at least one publisher's chain head
// could not be decoded, so that publisher's tail was never checked. It is a
// separate sentinel from errAuditFailed because the report above it is
// silent on this: nothing was found wrong with the entries that ARE stored;
// what failed is that we cannot tell whether entries are missing from the end.
var errAuditAnchorMalformed = errors.New("audit verification failed: undecodable chain head, tail-truncation unverified for publisher(s)")

// buildKeySet assembles the verifier's trusted key set from the session's
// own audit key (status) and the component publisher-keys ConfigMap. Keys
// are registered by (publisher, keyID) — the session key under the session
// publisher, component keys under theirs — so the offline verifier resolves
// keys exactly as the write-time WriteVerifier does (a key trusted for one
// publisher cannot verify another's chain). The registry enforces the
// content-addressing invariant (keyID == KeyID(pub)) on load, so a tampered
// ConfigMap entry is rejected with a warning rather than silently trusted —
// and a rejected key must not abort verification of the rest.
func buildKeySet(ctx context.Context, b *kube.Bundle, sess *spiceboxv1alpha1.AgentSession, errOut io.Writer) *publisherkeys.Registry {
	reg := publisherkeys.New()

	if sess.Status.AuditPublicKey != "" && sess.Status.AuditKeyID != "" {
		pub, err := provenance.DecodePubKey(sess.Status.AuditPublicKey)
		if err != nil {
			fmt.Fprintf(errOut, "warning: session audit public key invalid: %v\n", err)
		} else if aErr := reg.Add(provenance.SessionPublisher(sess.Namespace, sess.Name), sess.Status.AuditKeyID, pub); aErr != nil {
			fmt.Fprintf(errOut, "warning: session audit key rejected: %v\n", aErr)
		}
	}

	var cm corev1.ConfigMap
	err := b.Controller.Get(ctx, client.ObjectKey{Namespace: publisherKeysNamespace, Name: publisherKeysConfigMapName}, &cm)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(errOut, "warning: read %s ConfigMap: %v (component keys unavailable)\n", publisherKeysConfigMapName, err)
		}
		return reg
	}
	rawJSON := cm.Data[publisherKeysConfigMapKey]
	if rawJSON == "" {
		return reg
	}
	var compEntries []publisherkeys.Entry
	if err := json.Unmarshal([]byte(rawJSON), &compEntries); err != nil {
		fmt.Fprintf(errOut, "warning: decode %s.%s: %v (component keys unavailable)\n", publisherKeysConfigMapName, publisherKeysConfigMapKey, err)
		return reg
	}
	for _, rejErr := range reg.Load(compEntries) {
		fmt.Fprintf(errOut, "warning: component key rejected; not trusted: %v\n", rejErr)
	}
	return reg
}

// addWitnessedSessionKeys registers, under sessionPublisher, every audit key a
// key ALREADY in reg has attested inside the scope's own audit_key records.
//
// The problem it answers: a session's audit key lives in a Secret owned by its
// AgentSession, with the public half on that CR's status, so deleting the CR
// destroys both. The append-only records that key signed survive — they are
// permanent and the scope is keyed by namespace/name — so the next AgentSession
// of that name verifies a chain whose earlier entries it holds no key for, and
// every one of them reads as unknown-key. The operator therefore witnesses each
// binding in the scope as it mints the key (pkg/memory/kinds/auditkey), signed
// under its own publisher, whose key is in the durable publisher-keys ConfigMap.
//
// The trust chain is the whole point, so a witness is believed only when reg can
// already verify the record that carries it: an unsigned record, one signed by an
// unregistered publisher, or one whose signature does not check out installs
// NOTHING, and says so on errOut. Registry.Add supplies the last guard — it
// refuses a keyID that is not the content address of the key — so a record whose
// stated keyID and key disagree cannot bind either.
//
// A witness published under sessionPublisher itself is refused BEFORE its
// signature is checked, for two reasons that arrive at the same rule. It is the
// invariant the Kind states outright (pkg/memory/kinds/auditkey: a session may
// never author its own key binding, because a self-attested key proves nothing)
// — in the cluster a write-door rule, since auditkey is ComponentWritten, but
// this is an OFFLINE reader of a store it is being asked to distrust, so it
// cannot inherit that door and has to say the same thing itself.
//
// It is also what keeps "already in reg" honest. This loop writes into the very
// registry the next iteration consults, and sessionPublisher is the only
// publisher it ever registers a key for — so without this rule the first witness
// must be component-attested and every one after it could be signed by a key an
// earlier witness had just installed. Refusing the session's own publisher makes
// that growth unreachable rather than merely unexploited.
//
// Note what this does NOT do: it never registers a key for a publisher other
// than sessionPublisher, so a witness cannot mint trust for a component.
func addWitnessedSessionKeys(reg *publisherkeys.Registry, sessionPublisher string, entries []memory.Entry, errOut io.Writer) {
	for _, e := range entries {
		if e.Kind != auditkey.KindName {
			continue
		}
		if e.Provenance != nil && e.Provenance.Publisher == sessionPublisher {
			fmt.Fprintf(errOut, "warning: witnessed audit key %q is self-attested (published by %q, the session it would mint a key for); not trusted\n",
				e.ID, sessionPublisher)
			continue
		}
		if err := provenance.VerifyEntrySignature(reg, e); err != nil {
			fmt.Fprintf(errOut, "warning: witnessed audit key %q is not attestable; not trusted: %v\n", e.ID, err)
			continue
		}
		binding, err := auditkey.Decode(e)
		if err != nil {
			fmt.Fprintf(errOut, "warning: witnessed audit key %q could not be decoded; not trusted: %v\n", e.ID, err)
			continue
		}
		pub, err := provenance.DecodePubKey(binding.PubKey)
		if err != nil {
			fmt.Fprintf(errOut, "warning: witnessed audit key %q carries an invalid public key; not trusted: %v\n", e.ID, err)
			continue
		}
		if err := reg.Add(sessionPublisher, binding.KeyID, pub); err != nil {
			fmt.Fprintf(errOut, "warning: witnessed audit key %q rejected; not trusted: %v\n", e.ID, err)
		}
	}
}

// parseAnchors decodes the status.AuditChainHeads map (publisher →
// "seq:lastHash") into per-publisher ChainHead anchors, returning the
// publishers whose value could not be decoded alongside them.
//
// A malformed value is skipped rather than aborting — the other publishers'
// chains are still worth verifying — but it is NEVER skipped silently. The
// anchor is the only input that makes VerifyChain run its tail-truncation
// block at all (that whole block is wrapped in `if anchor != nil`), so a
// dropped anchor turns off exactly the detection this command exists to
// perform. The operator writes these values as fmt.Sprintf("%d:%s", …), so a
// malformed one means tampering or corruption — the threat, not a
// tolerable input. Callers must both surface the warning and fail the run;
// see auditExitError.
func parseAnchors(heads map[string]string, errOut io.Writer) (map[string]*provenance.ChainHead, []string) {
	if len(heads) == 0 {
		return nil, nil
	}
	out := make(map[string]*provenance.ChainHead, len(heads))
	var malformed []string
	bad := func(publisher, v, why string) {
		fmt.Fprintf(errOut, "warning: chain head for publisher %q is malformed (%s): %q — tail-truncation detection is DISABLED for this publisher\n", publisher, why, v)
		malformed = append(malformed, publisher)
	}
	for publisher, v := range heads {
		i := strings.IndexByte(v, ':')
		if i <= 0 {
			bad(publisher, v, `expected "<seq>:<hash>"`)
			continue
		}
		seq, err := strconv.ParseUint(v[:i], 10, 64)
		if err != nil {
			bad(publisher, v, "sequence is not a number")
			continue
		}
		out[publisher] = &provenance.ChainHead{Seq: seq, LastHash: v[i+1:]}
	}
	sort.Strings(malformed)
	return out, malformed
}

// auditExitError decides the command's exit status. A hard finding fails, and
// so does a malformed chain head: an undecodable anchor removes the truncation
// check for that publisher, so exiting 0 on one would report "verified" for a
// session whose tail nothing checked.
func auditExitError(reports []provenance.Report, malformedAnchors []string) error {
	if hardFindingCount(reports) > 0 {
		return errAuditFailed
	}
	if len(malformedAnchors) > 0 {
		return fmt.Errorf("%w: %s", errAuditAnchorMalformed, strings.Join(malformedAnchors, ", "))
	}
	return nil
}

// buildAuditReport groups entries by publisher, verifies each chain,
// and returns a per-publisher result. Signed entries are routed to the
// appropriate publisher's VerifyChain call; nil-provenance entries are
// bucketed into a synthetic "unsigned" Report (a warning, never a hard
// finding) so they are not double-counted against any signed publisher's
// pass.
//
// The publisher list is the UNION of the publishers that have stored entries
// and the publishers that have a recorded chain-head anchor — never the
// entries alone. Deleting every append-only entry of a publisher must not be
// quieter than deleting one of them: with an entries-only list, a wiped
// publisher is simply absent, contributes no finding, and the command exits 0
// on the maximal-tampering input. The anchor is the surviving evidence that
// the chain existed, and VerifyChain's truncation check is written to run
// against an empty entry set (max seq 0 < anchor seq → tail-truncated).
//
// Consequence, accepted deliberately: an ephemeral memory backend that drops
// entries after heads were anchored (an inmem operator restart in local dev)
// now reports tail-truncated. That is a true positive — the entries the
// anchor attests to really are gone — and it is NOT suppressed by backend
// mode. A tamper-evidence check relaxed in the one mode everybody develops
// against is a check nobody exercises before it has to hold in production.
func buildAuditReport(verifier *provenance.Verifier, entries []memory.Entry, anchors map[string]*provenance.ChainHead) []provenance.Report {
	signedByPublisher := map[string][]memory.Entry{}
	var unsigned []memory.Entry
	for _, e := range entries {
		if e.Provenance == nil {
			unsigned = append(unsigned, e)
			continue
		}
		signedByPublisher[e.Provenance.Publisher] = append(signedByPublisher[e.Provenance.Publisher], e)
	}

	publishers := make([]string, 0, len(signedByPublisher))
	for p := range signedByPublisher {
		publishers = append(publishers, p)
	}
	for p := range anchors {
		if _, seen := signedByPublisher[p]; !seen {
			publishers = append(publishers, p)
		}
	}
	sort.Strings(publishers)

	var reports []provenance.Report
	for _, publisher := range publishers {
		// Pass only this publisher's signed entries; nil-provenance entries
		// are handled separately, so VerifyChain sees no unsigned noise here.
		reports = append(reports, verifier.VerifyChain(publisher, signedByPublisher[publisher], anchors[publisher]))
	}

	if len(unsigned) > 0 {
		rep := provenance.Report{Publisher: unsignedPublisher}
		for _, e := range unsigned {
			rep.Findings = append(rep.Findings, provenance.Finding{
				Verdict: provenance.VerdictUnsigned,
				Kind:    e.Kind,
				ID:      e.ID,
				Detail:  "unsigned (pre-provenance or unsigned writer)",
			})
		}
		reports = append(reports, rep)
	}
	return reports
}

// hardFindingCount counts findings that must fail verification: every
// verdict EXCEPT unsigned. Unsigned entries are a warning.
func hardFindingCount(reports []provenance.Report) int {
	n := 0
	for _, rep := range reports {
		for _, f := range rep.Findings {
			if f.Verdict != provenance.VerdictUnsigned {
				n++
			}
		}
	}
	return n
}

func renderAuditReports(out io.Writer, th *tui.Theme, sessionName string, reports []provenance.Report) {
	fmt.Fprintf(out, "Audit verification for session %q\n\n", sessionName)

	t := tui.NewTable(th, "PUBLISHER", "VERIFIED OK", "FINDINGS")
	var hard, unsignedCount int
	for _, rep := range reports {
		for _, f := range rep.Findings {
			if f.Verdict == provenance.VerdictUnsigned {
				unsignedCount++
			} else {
				hard++
			}
		}
		t.Row(rep.Publisher, strconv.Itoa(rep.OK), strconv.Itoa(len(rep.Findings)))
	}
	fmt.Fprint(out, t.Render())

	// Per-publisher finding detail.
	for _, rep := range reports {
		if len(rep.Findings) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n%s:\n", rep.Publisher)
		for _, f := range rep.Findings {
			loc := f.Kind
			if f.ID != "" {
				loc = f.Kind + "/" + f.ID
			}
			if loc == "" {
				loc = fmt.Sprintf("seq %d", f.Seq)
			}
			fmt.Fprintf(out, "  [%s] %s: %s\n", f.Verdict, loc, f.Detail)
		}
	}

	fmt.Fprintln(out)
	switch {
	case hard > 0:
		fmt.Fprintf(out, "FAILED: %d hard finding(s)", hard)
		if unsignedCount > 0 {
			fmt.Fprintf(out, ", %d unsigned (pre-provenance or unsigned writer)", unsignedCount)
		}
		fmt.Fprintln(out)
	case unsignedCount > 0:
		fmt.Fprintf(out, "OK: chains verify; %d unsigned (pre-provenance or unsigned writer) — warning only\n", unsignedCount)
	default:
		fmt.Fprintln(out, "OK: all chains verify, no findings")
	}
}
