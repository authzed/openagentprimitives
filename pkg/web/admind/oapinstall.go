package admind

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
	"github.com/authzed/openagentprimitives/pkg/web/admind/health"
)

// systemNamespaces are refused as install targets (defense-in-depth on top of
// the pkg/platform/oap/install ownership guard): installing an agent graph into a
// Kubernetes system namespace or the control plane's own namespace has no
// legitimate use and is exactly where a confused-deputy payload would aim to
// clobber shared infra (the audit trust-root ConfigMap, coredns, …).
// health.Namespace is the operator/control-plane namespace admind itself runs
// in — reused here so this stays in sync with the one place that value lives.
var systemNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
	health.Namespace:  true,
}

// maxOapInstallBody caps the total request body POST /agents/oap-install will
// read — generous enough for a real .oap upload (manifests + small assets)
// without letting an unbounded request pin admind's memory. oap.Unpack's own
// tar-extraction caps (pkg/platform/oap/layout.go) still apply on top of this once the
// bytes are in hand.
const maxOapInstallBody = 64 << 20 // 64 MiB

// oapInstallJSONRequest is the ref-based install shape: POST a JSON body
// naming a registry ref instead of uploading a .oap file. Values answers the
// bundle's install questions non-interactively (string form — the same shape
// install.Resolve's --set/--values path accepts; a QInt/QBool question's
// string is coerced by Resolve itself).
type oapInstallJSONRequest struct {
	// Ref is the OCI registry reference to pull the bundle from.
	Ref string `json:"ref"`
	// PlainHTTP is REFUSED on this surface.
	//
	// It relaxes the OCI SSRF guard's private-range and loopback rule (see
	// pkg/platform/oap/oci.guardHost rule 3), on the reasoning that a caller who has
	// opted out of TLS for a local dev registry has already made that trust
	// decision themselves. Over an HTTP API the caller is a REMOTE one, and
	// letting them set it turns "I am running against my laptop registry" into
	// "point the operator at anything inside the cluster network".
	//
	// Kept as a field rather than removed so a body that sets it is REFUSED by
	// name -- a local-dev script that relied on it is told, instead of pulling
	// over TLS and failing later with a confusing certificate error against a
	// plain-HTTP registry.
	PlainHTTP bool `json:"plainHttp"`
	// Namespace is the install target; systemNamespaces are refused.
	Namespace string `json:"namespace"`
	// Name overrides the bundle's own agent name; empty keeps the manifest's.
	Name string `json:"name"`
	// Values answers the bundle's install questions; a missing REQUIRED answer
	// with no manifest default is a 400 carrying the question schema.
	Values map[string]string `json:"values"`
	// Adopt names pre-existing objects ("Kind/Name") this install may seize —
	// see oapInstallParams.Adopt.
	Adopt []string `json:"adopt"`
}

// oapInstallResponse is the 200 body for a completed install.
type oapInstallResponse struct {
	// AgentPath is empty for the root and addresses a dependency otherwise.
	AgentPath string `json:"agentPath,omitempty"`
	// Name is the install's resolved agent name, which may differ from the
	// request's when the bundle's own manifest won.
	Name string `json:"name"`
	// AppliedKinds are the CR kinds actually applied, not the ones attempted.
	AppliedKinds []string `json:"appliedKinds"`
	// SecretsCreated counts Secrets minted for this install.
	SecretsCreated int `json:"secretsCreated"`
	// Warnings are NON-FATAL notices (an absent shared cluster dependency, say)
	// the caller must surface, never drop.
	Warnings []string `json:"warnings,omitempty"`
	// SkillClones are the external repositories this install cloned.
	SkillClones []oap.SkillClone `json:"skillClones,omitempty"`
	// Adopted is every pre-existing object this install seized, as sorted
	// "Kind/Name" keys — empty on an install that created everything fresh.
	Adopted []string `json:"adopted,omitempty"`
	// Channels is one row per channel the bundle declares, exactly as the
	// form-shaped 400s carry it — the kind's own setup fields, what this
	// install decided, and the token to submit the answers against.
	//
	// It is on the SUCCESS response and not only on the 400s because a channel
	// is created AFTER the agent it belongs to, and an install whose manifest
	// questions were all answered up front never sees a 400 at all. Without it
	// that install lands an agent with no route to the form its channels need,
	// which is precisely the "the UI can create a channel, except it cannot"
	// shape this response exists to avoid.
	Channels []oapInstallChannel `json:"channels,omitempty"`
	// Agents recursively carries private dependency results. Root fields stay
	// in their original locations for backward compatibility.
	Agents []oapInstallResponse `json:"agents,omitempty"`
}

// oapInstallMissingQuestionsResponse is the 400 body for an install that
// can't proceed non-interactively because one or more REQUIRED questions
// have no answer and no manifest default — the admin UI renders Questions'
// full typed schema (type, prompt, enum options, required) to build the
// missing-field form and re-POST with them filled in. A Question never
// carries an answer value — only its shape — so this body is safe to log
// and return even for a secret-typed question.
type oapInstallMissingQuestionsResponse struct {
	// Error is the human summary; Questions carries the actionable detail.
	Error string `json:"error"`
	// Questions is each unanswered question's full typed SHAPE — never an
	// answer value, so this body is safe to log even for a secret question.
	Questions     []oap.Question `json:"questions"`
	questionPaths []string
	// SkillClones tells the UI which external repositories installing this
	// bundle will clone, so the missing-question form can show that alongside
	// the fields to fill in (the operator consents to the fetch before install).
	SkillClones []oap.SkillClone `json:"skillClones,omitempty"`
	// Warnings carries the capacity hook's notices alongside the unanswered
	// questions — the one path where they name the SPECIFIC value the form is
	// about to pre-fill, so dropping them here discards the most actionable
	// text this response can carry. Empty for an ordinary missing manifest
	// question.
	Warnings []string `json:"warnings,omitempty"`
	// Channels is one row per channel the bundle declares (requires.channels):
	// the kind's own setup fields, what this install already decided and where
	// each decision came from, or — for a channel this form cannot set up —
	// why and what does work. Empty for every bundle that declares none, which
	// is every bundle written before requires.channels existed.
	//
	// Like Questions, a row carries a field's SHAPE and not an answer; see
	// oapInstallChannelSeed.Value for the one place a value appears and the
	// rule that keeps it safe in a logged body.
	Channels []oapInstallChannel `json:"channels,omitempty"`
	// Conflicts may accompany missing questions when another graph node was
	// otherwise fully preparable in the same aggregate read-only pass.
	Conflicts []oapInstallConflict `json:"conflicts,omitempty"`
}

func (r oapInstallMissingQuestionsResponse) MarshalJSON() ([]byte, error) {
	type addressedQuestion struct {
		AgentPath string `json:"agentPath,omitempty"`
		oap.Question
	}
	questions := make([]addressedQuestion, 0, len(r.Questions))
	for i, question := range r.Questions {
		path := ""
		if i < len(r.questionPaths) {
			path = r.questionPaths[i]
		}
		questions = append(questions, addressedQuestion{AgentPath: path, Question: question})
	}
	return json.Marshal(struct {
		Error       string               `json:"error"`
		Questions   []addressedQuestion  `json:"questions"`
		SkillClones []oap.SkillClone     `json:"skillClones,omitempty"`
		Warnings    []string             `json:"warnings,omitempty"`
		Channels    []oapInstallChannel  `json:"channels,omitempty"`
		Conflicts   []oapInstallConflict `json:"conflicts,omitempty"`
	}{r.Error, questions, r.SkillClones, r.Warnings, r.Channels, r.Conflicts})
}

// oapInstallConflict is one pre-existing object the install would seize,
// mirroring install.Conflict for the wire. ClusterScoped is deliberately NOT
// carried: a cluster-scoped collision is never adoptable, so it stays a plain
// error the UI shows as text rather than a tickable row.
type oapInstallConflict struct {
	AgentPath string `json:"agentPath,omitempty"`
	Kind      string `json:"kind"`
	// Namespace is absent for a cluster-scoped collision.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Secret marks a Secret collision, which the UI warns about differently:
	// adopting one overwrites credential material.
	Secret bool `json:"secret,omitempty"`
}

// oapInstallConflictsResponse is the 409 body for an install blocked by
// pre-existing objects it neither created nor was told to adopt. The UI
// renders one tickable row per conflict and re-POSTs with `adopt` naming the
// ones the operator approved — that re-POST is the approval; the server never
// prompts.
type oapInstallConflictsResponse struct {
	// Error is the human summary; Conflicts carries the actionable detail.
	Error string `json:"error"`
	// Conflicts is one row per adoptable pre-existing object.
	Conflicts []oapInstallConflict `json:"conflicts"`
	// SkillClones are the repositories this install would clone, shown so the
	// operator consents to the fetch alongside the adoptions.
	SkillClones []oap.SkillClone `json:"skillClones,omitempty"`
	// Channels and Warnings retain every other graph decision discovered in
	// the same read-only pass, even when conflicts determine the status code.
	Channels []oapInstallChannel `json:"channels,omitempty"`
	Warnings []string            `json:"warnings,omitempty"`
}

// handleOapInstall installs a .oap agent container from the admin UI: load
// the bundle (an uploaded file or a pulled registry ref), fail-closed
// preflight it, resolve its install questions non-interactively (a missing
// required answer is reported, never prompted for), and apply it to the
// cluster. Every failure is wrapped with enough context to act on; a secret
// VALUE is never included in a response or a log line — only Secret
// name/key, which carry no confidential material.

// toWireConflicts converts install.Conflict (the library's internal shape,
// which also carries ClusterScoped — never adoptable, so never rendered as a
// tickable row) to the wire shape the admin UI unmarshals.
func toWireConflicts(cs []install.Conflict) []oapInstallConflict {
	out := make([]oapInstallConflict, 0, len(cs))
	for _, c := range cs {
		out = append(out, oapInstallConflict{Kind: c.Kind, Namespace: c.Namespace, Name: c.Name, Secret: c.Secret})
	}
	return out
}

// validateInstallTarget checks the caller-supplied namespace (required) and
// optional install name against Kubernetes' own naming rules BEFORE any
// cluster write, so a malformed value returns a clean 400 here rather than
// surfacing as an opaque 500 from the apiserver deep inside install.Install. A
// namespace is a DNS-1123 label; the install name becomes a "<name>-" prefix
// on every bundled CR's name (a DNS-1123 subdomain), so it is validated as a
// subdomain when present. Empty name is valid — install defaults it to the
// bundled AgentClass's own name.
func validateInstallTarget(namespace, name string) error {
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return fmt.Errorf("invalid namespace %q: %s", namespace, strings.Join(errs, "; "))
	}
	if systemNamespaces[namespace] {
		return fmt.Errorf("cannot install into a system namespace")
	}
	if name != "" {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return fmt.Errorf("invalid name %q: %s", name, strings.Join(errs, "; "))
		}
	}
	return nil
}

// oapInstallParams is one parsed install request, from either the multipart
// (uploaded file) or JSON (registry ref) shape.
type oapInstallParams struct {
	// FileBytes is the uploaded .oap; empty on the ref-based path.
	FileBytes []byte
	// Ref is the registry reference; empty on the upload path.
	Ref string
	// PlainHTTP stays FALSE on this surface -- the JSON decoder refuses a body
	// that sets it. Kept on the struct because loadOapInstallBundle takes it and
	// a local caller of that helper may legitimately pass true; see
	// oapInstallJSONRequest.PlainHTTP.
	PlainHTTP bool
	Namespace string
	Name      string
	Values    map[string]string
	// Adopt names pre-existing objects ("Kind/Name") this install may seize,
	// populated by the UI's confirm step after a 409. There is NO blanket
	// "adopt everything" over the wire: the browser names each object the
	// operator ticked, so a bundle that grows a new colliding CR can never be
	// adopted by a stale confirmation.
	Adopt []string
}

// parseOapInstallRequest reads either a multipart/form-data request (a "file"
// upload field plus "namespace"/"name"/"values"/"adopt" text fields, the
// latter two a JSON object/array) or a JSON body
// ({ref, plainHttp, namespace, name, values, adopt}), dispatching on
// Content-Type. Exactly one of the returned FileBytes/Ref is meaningful —
// loadOapInstallBundle decides which per its own precedence.
func parseOapInstallRequest(r *http.Request) (*oapInstallParams, error) {
	// Content-Type is case-insensitive per RFC 7231; lowercase before the
	// prefix match so a "Multipart/Form-Data" header isn't misrouted to the
	// JSON branch.
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/form-data") {
		// 32 MiB is the in-memory threshold ParseMultipartForm buffers before
		// spilling remaining parts to a temp file; maxOapInstallBody (already
		// wrapping r.Body) is the hard cap on the whole request.
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, fmt.Errorf("parse multipart form: %w", err)
		}
		f, _, ferr := r.FormFile("file")
		if ferr != nil {
			return nil, fmt.Errorf(`missing "file" upload: %w`, ferr)
		}
		defer f.Close()
		fileBytes, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read uploaded file: %w", err)
		}
		p := &oapInstallParams{
			FileBytes: fileBytes,
			Namespace: r.FormValue("namespace"),
			Name:      r.FormValue("name"),
		}
		if raw := r.FormValue("values"); raw != "" {
			if jerr := json.Unmarshal([]byte(raw), &p.Values); jerr != nil {
				return nil, fmt.Errorf(`parse "values" field as JSON: %w`, jerr)
			}
		}
		if raw := r.FormValue("adopt"); raw != "" {
			if jerr := json.Unmarshal([]byte(raw), &p.Adopt); jerr != nil {
				return nil, fmt.Errorf(`parse "adopt" field as JSON: %w`, jerr)
			}
		}
		return p, nil
	}

	var body oapInstallJSONRequest
	if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil {
		return nil, fmt.Errorf("parse JSON body: %w", derr)
	}
	if body.Ref == "" {
		return nil, fmt.Errorf(`JSON body must set "ref"`)
	}
	// PlainHTTP is NOT carried through. See oapInstallJSONRequest.PlainHTTP:
	// it relaxes the OCI SSRF guard's private-range/loopback rule, which is a
	// trust decision only a local operator can make for their own laptop
	// registry. Over this API the caller is remote.
	//
	// Refused rather than silently dropped, so a local-dev script that relied
	// on it is told, instead of pulling over TLS and failing later with a
	// confusing certificate error against a plain-HTTP registry.
	if body.PlainHTTP {
		return nil, fmt.Errorf(`"plainHttp" is not accepted over the admin API: it relaxes the registry SSRF guard for private and loopback addresses, which is a decision only a local install can make; use the CLI, or allow-list the registry host`)
	}
	return &oapInstallParams{
		Ref:       body.Ref,
		Namespace: body.Namespace,
		Name:      body.Name,
		Values:    body.Values,
		Adopt:     body.Adopt,
	}, nil
}

// loadOapInstallBundle decodes an uploaded .oap's bytes when present,
// otherwise pulls ref from a registry (oci.Pull is SSRF-guarded — see
// pkg/platform/oap/oci — so an admin-supplied ref can't be turned into an internal
// probe). Returns the decoded Bundle plus the provenance triple
// (sourceKind/sourceRef/sourceDigest) install.InstallOpts stamps onto the
// applied AgentClass.
func loadOapInstallBundle(ctx context.Context, fileBytes []byte, ref string, plainHTTP bool) (b *oap.Bundle, sourceKind, sourceRef, sourceDigest string, err error) {
	if len(fileBytes) > 0 {
		b, err = oap.Unpack(fileBytes)
		if err != nil {
			return nil, "", "", "", fmt.Errorf("unpack uploaded .oap: %w", err)
		}
		digest, derr := oap.Digest(fileBytes)
		if derr != nil {
			return nil, "", "", "", fmt.Errorf("digest uploaded .oap: %w", derr)
		}
		return b, "file", "", digest, nil
	}
	if ref == "" {
		return nil, "", "", "", fmt.Errorf(`must provide either an uploaded "file" or a registry "ref"`)
	}
	packed, digest, err := oci.Pull(ctx, ref, oci.Options{PlainHTTP: plainHTTP})
	if err != nil {
		return nil, "", "", "", fmt.Errorf("pull %s: %w", ref, err)
	}
	b, err = oap.Unpack(packed)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("unpack %s: %w", ref, err)
	}
	return b, "registry", ref, digest, nil
}

// missingRequiredQuestions mirrors install.Resolve's own non-interactive
// fallback (an unanswered question is satisfied by its manifest Default
// before being flagged missing) to recover the exact set of still-unanswered
// REQUIRED questions for the 400 response — the same set whose absence made
// Resolve itself fail. Returns nil when nothing is missing (Resolve's error,
// if any, came from something else — an unknown key, a CEL validation
// failure — and gets a plain error response instead). The full Question is
// returned (not just its name) so the admin UI can render a typed field
// (string/int/bool/enum/secret/resourceList) without a second round trip.
func missingRequiredQuestions(qs []oap.Question, values map[string]string) []oap.Question {
	var missing []oap.Question
	for _, q := range qs {
		if _, ok := values[q.Name]; ok {
			continue
		}
		if q.Default != nil {
			continue
		}
		if q.IsRequired() {
			missing = append(missing, q)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
	return missing
}
