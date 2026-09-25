// Package blockcapture drives OAP's real Slack channel kind against an
// in-process Slack API stub and dumps the exact Block Kit JSON the sender
// serializes. The showcase demo/docs system renders those fixtures so a demo
// shows the blocks OAP really sends, not a hand-authored approximation — and a
// change to the slack kind's rendering shows up as a fixture diff.
//
// It uses only exported seams. slack.InstallTestTransport swaps the real slack-go
// client for one pointed at an httptest server; the stub records the "blocks"
// form value, which is the exact wire JSON slack-go builds at request time. This
// is captured at the HTTP boundary rather than from fakeslack, because fakeslack
// stores message text only and never the Block Kit payload.
package blockcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"

	slackapi "github.com/slack-go/slack"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	slackkind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Post is one message the sender sent, with its Block Kit JSON (empty for a
// status-only call).
type Post struct {
	Endpoint string          `json:"endpoint"`
	Blocks   json.RawMessage `json:"blocks,omitempty"`
	Text     string          `json:"text,omitempty"`
}

type recorder struct {
	mu    sync.Mutex
	posts []Post
}

func (rec *recorder) add(p Post) {
	rec.mu.Lock()
	rec.posts = append(rec.posts, p)
	rec.mu.Unlock()
}

func slackStub(rec *recorder) *httptest.Server {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	record := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p := Post{Endpoint: r.URL.Path, Text: r.FormValue("text")}
		if b := r.FormValue("blocks"); b != "" {
			p.Blocks = json.RawMessage(b)
		}
		rec.add(p)
		write(w, `{"ok":true,"channel":"C0DEMO","ts":"1700000000.000100","message_ts":"1700000000.000100"}`)
	}
	mux.HandleFunc("/api/chat.postMessage", record)
	mux.HandleFunc("/api/chat.postEphemeral", record)
	mux.HandleFunc("/api/chat.update", record)
	mux.HandleFunc("/api/conversations.open", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"ok":true,"channel":{"id":"D0DEMO"}}`)
	})
	mux.HandleFunc("/api/assistant.threads.setStatus", func(w http.ResponseWriter, _ *http.Request) { write(w, `{"ok":true}`) })
	mux.HandleFunc("/api/assistant.threads.setTitle", func(w http.ResponseWriter, _ *http.Request) { write(w, `{"ok":true}`) })
	// Any other Slack method the sender happens to touch: succeed with a bare ok.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { write(w, `{"ok":true}`) })
	return httptest.NewServer(mux)
}

// Capture drives one or more envelopes, in order, through a SINGLE instance of
// the named sub-channel's sender ("" or "message" is the main sender;
// "interaction" is the approval/notice sender) and returns every Slack post the
// sender made, Block Kit JSON included. The single-instance sequence matters for
// a resolved approval: the interaction sender caches the prompt's detail when the
// request is sent, then reuses it to edit the card in place when the applied
// envelope arrives — send them through separate senders and the resolved card
// degrades to a verdict-only chip.
func Capture(ctx context.Context, subChannel string, sess channelkinds.SessionInfo, envs ...channelevents.Envelope) ([]Post, error) {
	rec := &recorder{}
	srv := slackStub(rec)
	defer srv.Close()

	real := slackapi.New("xoxb-fake", slackapi.OptionAPIURL(srv.URL+"/api/"))
	reset := slackkind.InstallTestTransport(real, fakeslack.NewSocketSource())
	defer reset()

	k := &slackkind.Kind{}
	var sender channelkinds.Sender
	switch subChannel {
	case "", "message":
		sender = k.NewSender(channelkinds.Deps{})
	default:
		sender = k.SubChannelSender(subChannel, channelkinds.Deps{})
	}
	if sender == nil {
		return nil, fmt.Errorf("blockcapture: nil sender for sub-channel %q", subChannel)
	}
	for _, env := range envs {
		if _, err := sender.Send(ctx, sess, env); err != nil {
			return rec.posts, fmt.Errorf("blockcapture: send %q: %w", env.Kind, err)
		}
	}
	return rec.posts, nil
}

// FirstBlocks returns the Block Kit JSON of the first post that carried any.
func FirstBlocks(posts []Post) json.RawMessage {
	return pickBlocks(posts, "")
}

// pickBlocks returns the blocks of the first post carrying any; when endpoint is
// non-empty, it restricts to posts to that Slack method (e.g. "/api/chat.update"
// for a resolved card that edits the original in place).
func pickBlocks(posts []Post, endpoint string) json.RawMessage {
	for _, p := range posts {
		if len(p.Blocks) == 0 {
			continue
		}
		if endpoint == "" || p.Endpoint == endpoint {
			return p.Blocks
		}
	}
	return nil
}

// Specimen is one named capture: an envelope sequence (sent through one sender)
// + session + the sub-channel that renders it. Pick selects which post's blocks
// to write: "" = first post with blocks; an endpoint like "/api/chat.update"
// selects the in-place edit (a resolved approval).
type Specimen struct {
	Name       string
	SubChannel string
	Session    channelkinds.SessionInfo
	Envelopes  []channelevents.Envelope
	Pick       string
}

func demoSession(channelID, threadTS string) channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: "default",
		Name:      "demo-sess",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name:     "slack-demo",
			Kind:     "slack",
			External: map[string]string{"channel_id": channelID, "thread_ts": threadTS},
		},
	}
}

// planGateCardFields builds the What/Why/When/Who fields of a plan-gate approval
// card, mirroring the shape pkg/agent/runner planGateFields+planGateItems emit
// from a frozen plan. The permission lines carry the blast-radius tone
// (readonly/readwrite/external) and the raw handle as a hint; the resource lines
// carry the repo with its GitHub icon + link. Content models codebot's real
// slots (git_repo read/write/push, github_repo read/write-external); the
// rendering is OAP's real interaction renderer.
func planGateCardFields() []channelevents.InteractionField {
	read := func(text, hint string) channelevents.InteractionItem {
		return channelevents.InteractionItem{Text: text, Tone: channelevents.ToneReadonly, Hint: hint}
	}
	write := func(text, hint string) channelevents.InteractionItem {
		return channelevents.InteractionItem{Text: text, Tone: channelevents.ToneReadwrite, Hint: hint}
	}
	external := func(text, hint string) channelevents.InteractionItem {
		return channelevents.InteractionItem{Text: text, Tone: channelevents.ToneExternal, Detail: "leaves this session", Hint: hint}
	}
	// The slotted repo: same instance reached in every phase. Detail == Href by
	// construction (text == href), Icon from the resource type's declared display.
	repo := channelevents.InteractionItem{
		Text: "reaches GitHub repository", Detail: "acme/widget",
		Icon: "github", Href: "https://github.com/acme/widget",
	}
	phases := []channelevents.InteractionItem{
		{Text: "Phase 1 — Clone the repository and read it", Items: []channelevents.InteractionItem{
			read("read files and history in the checkout", "perm:read:git_repo"),
			read("read the repository on GitHub", "perm:read:github_repo"),
			repo,
		}},
		{Text: "Phase 2 — Make the change and commit locally", Items: []channelevents.InteractionItem{
			write("write files in the checkout and commit", "perm:write:git_repo"),
			repo,
		}},
		{Text: "Phase 3 — Push the branch and open a pull request", Items: []channelevents.InteractionItem{
			write("push the branch to GitHub", "perm:push:git_repo"),
			external("open a pull request on GitHub", "perm:write:github_repo"),
			repo,
		}},
		{Text: "Approving covers every phase above.", Tone: channelevents.ToneMuted},
	}
	return []channelevents.InteractionField{
		{Label: "What", Value: "Clone acme/widget, make the change, then push and open a pull request.", Items: phases},
		{Label: "Why (the agent's words)", Value: "Fixing the flaky test in the payments suite and opening a PR for review."},
		{Label: "When", Value: "Now, for as long as this session runs."},
		{Label: "Who can approve", Value: "An owner of this session."},
	}
}

// Specimens is the fixed set of envelopes the demos render. Each yields one
// <Name>.json Block Kit fixture. Add a row here to capture a new surface.
func Specimens() ([]Specimen, error) {
	sess := demoSession("C0DEMO", "1700000000.000100")

	approval := channelevents.InteractionRequestPayload{
		Category:   categories.ToolApproval,
		RequestRef: "req-1",
		Lead:       "Deploy `hotfix-1.4.2` to production?",
		// The subject of the approval lives in the BODY (and fields), not only in
		// the Lead: the Lead is the pending status headline and is replaced by the
		// outcome when the card resolves, so detail carried solely in the Lead
		// would not survive into the resolved card.
		Body: "Deploying `hotfix-1.4.2` — restarts 12 pods and runs the post-deploy smoke suite.",
		Fields: []channelevents.InteractionField{
			{Label: "Cluster", Value: "prod-us-east"},
			{Label: "Pods", Value: "12"},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: identity.Kind("slack"), ExternalID: identity.RawExternalID("U_OWNER")}},
		},
	}
	approvalEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest, approval)
	if err != nil {
		return nil, err
	}

	// The resolved (approved) card is a chat.update of the request, so it must be
	// sent through the same sender after the request. DecidedBy is a raw slack id
	// so it renders as a native <@…> mention.
	appliedEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			Category:   categories.ToolApproval,
			RequestRef: "req-1",
			Outcome:    channelevents.OutcomeApproved,
			DecidedBy:  &channelevents.ExternalIdentity{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")},
		})
	if err != nil {
		return nil, err
	}

	// A participant-join approval: a new person asked to take part, and an owner
	// must allow it before their input is acted on. PermissionRequest is the
	// owner-gated "may I permit this" category.
	joinReq := channelevents.InteractionRequestPayload{
		Category:   categories.PermissionRequest,
		RequestRef: "join-1",
		Lead:       "Alex asked to join this session and help drive the rollout — allow them to participate?",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")}},
		},
	}
	joinReqEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest, joinReq)
	if err != nil {
		return nil, err
	}
	joinAppliedEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			Category:   categories.PermissionRequest,
			RequestRef: "join-1",
			Outcome:    channelevents.OutcomeApproved,
			DecidedBy:  &channelevents.ExternalIdentity{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")},
		})
	if err != nil {
		return nil, err
	}

	// A plan-gate approval: the agent proposes a multi-phase plan and one human
	// approval covers it. Category plan_phase, no Details/Resources; the phases
	// and the slotted repo live entirely in Fields.
	planReq := channelevents.InteractionRequestPayload{
		Category:   categories.PlanPhase,
		RequestRef: "plan-1",
		Lead:       "codebot wants approval to open a pull request on acme/widget.",
		Fields:     planGateCardFields(),
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")}},
		},
	}
	planReqEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest, planReq)
	if err != nil {
		return nil, err
	}
	planAppliedEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			Category:   categories.PlanPhase,
			RequestRef: "plan-1",
			Outcome:    channelevents.OutcomeApproved,
			DecidedBy:  &channelevents.ExternalIdentity{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")},
		})
	if err != nil {
		return nil, err
	}

	// A resource-owner-gated tool approval: a teammate asks for a company's
	// contacts, and the COMPANY's owner (not the session owner) must approve. The
	// Resources field names the company; DecideResourceOwners routes the approval
	// to crm_company#owner. Details carries the grant inputs + drives the
	// Show-Details button. Models a HubSpot company-owner approval flow.
	contactDetails, err := json.Marshal(channelevents.ToolApprovalDetails{
		Permission:      "contact_access",
		ResourceType:    "crm_company",
		ResourceID:      "circldot",
		ArgsHash:        "a1b2c3d4",
		StateImpact:     "readonly",
		ToolName:        "hubspot_search_crm_objects",
		ToolDescription: "Search HubSpot CRM records",
		Justification:   "Sam asked for Circldot's contacts to prepare the QBR deck.",
		ArgsJSON:        `{"objectType":"contacts","company":"Circldot"}`,
	})
	if err != nil {
		return nil, err
	}
	contactReq := channelevents.InteractionRequestPayload{
		Category:   categories.ToolApproval,
		RequestRef: "share-1",
		Lead:       "Approval needed",
		Fields: []channelevents.InteractionField{
			{Label: "Why", Value: "Sam asked for Circldot's contacts to prepare the QBR deck."},
			{Label: "What", Value: "Share Circldot's contacts."},
			{Label: "Tool", Value: "Search HubSpot CRM records"},
			{Label: "Permission", Value: "contact access on crm company"},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Details:   contactDetails,
		Resources: []channelevents.InteractionResourceRef{{Type: "crm_company", ID: "circldot", Permission: "owner"}},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")}},
		},
	}
	contactReqEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest, contactReq)
	if err != nil {
		return nil, err
	}
	contactAppliedEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			Category:   categories.ToolApproval,
			RequestRef: "share-1",
			Outcome:    channelevents.OutcomeApproved,
			DecidedBy:  &channelevents.ExternalIdentity{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")},
		})
	if err != nil {
		return nil, err
	}

	// The per-session identity choice (identityMode ask/dynamic): a DecideRequester
	// interaction with three buttons. `dynamic` adds a "Suggested:" body, styles the
	// recommended button primary, and routes the recommender's reason through an
	// inert Excerpt. Models pkg/agent/runner/identitygate.go.
	requester := channelevents.ExternalIdentity{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")}
	idChoiceActions := []channelevents.InteractionAction{
		{ID: "agent", Kind: channelevents.ActionKindDecision, Label: "Run as codebot"},
		{ID: "userPassthrough", Kind: channelevents.ActionKindDecision, Label: "Run as me"},
		{ID: "cancel", Kind: channelevents.ActionKindDecision, Label: "Cancel"},
	}
	idChoiceReqEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			Category:   categories.IdentityChoice,
			RequestRef: "idc-1",
			Lead:       "Which identity should codebot use for this session?",
			Actions:    idChoiceActions,
			Audience:   channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &requester},
		})
	if err != nil {
		return nil, err
	}
	dynActions := make([]channelevents.InteractionAction, len(idChoiceActions))
	copy(dynActions, idChoiceActions)
	dynActions[1].Style = channelevents.ActionStylePrimary // highlight the suggested "Run as me"
	idChoiceDynReqEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			Category:   categories.IdentityChoice,
			RequestRef: "idc-2",
			Lead:       "Which identity should codebot use for this session?",
			Body:       "Suggested: Run as me",
			Excerpt:    &channelevents.InteractionExcerpt{Label: "Why", Content: "You asked codebot to push to your fork and open a PR, which needs your GitHub identity."},
			Actions:    dynActions,
			Audience:   channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &requester},
		})
	if err != nil {
		return nil, err
	}

	// An information-leakage approval: the agent's drafted reply carries data a
	// recipient in the channel isn't permitted to see, so the DATA owner (not the
	// session owner) must approve the share. Category info_leakage, gated by
	// DecideResourceOwners over the tainted resource. Mirrors the per-datum egress
	// gate (pkg/agent/runner/host_approval.go infoLeakageLead + …WouldShareWithFields).
	infoLeakReq := channelevents.InteractionRequestPayload{
		Category:   categories.InfoLeakage,
		RequestRef: "leak-1",
		Lead:       "Here's Circldot's current MRR and renewal date for the QBR deck.",
		Fields: []channelevents.InteractionField{{
			Label:    "Would share with",
			Value:    "jamie (guest)",
			Mentions: []channelevents.ExternalIdentity{{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_GUEST")}},
		}},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Resources: []channelevents.InteractionResourceRef{{Type: "crm_company", ID: "circldot", Permission: "owner"}},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")}},
		},
	}
	infoLeakReqEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest, infoLeakReq)
	if err != nil {
		return nil, err
	}

	// A mid-turn queued-message ack: someone messaged while the agent was working,
	// so the message is queued with a one-click "Interrupt & Send Now". Category
	// queued_messages, routed to the requester. Mirrors channelsd pipeline.go.
	queuedReq := channelevents.InteractionRequestPayload{
		Category:   categories.QueuedMessages,
		RequestRef: "q-1",
		Lead:       "You messaged while I'm working — your message is queued.",
		Actions: []channelevents.InteractionAction{
			{ID: "interrupt", Label: "Interrupt & Send Now", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_OWNER")},
		},
	}
	queuedReqEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindInteractionRequest, queuedReq)
	if err != nil {
		return nil, err
	}

	msgEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "Rollout complete. `hotfix-1.4.2` is live on `prod-us-east` — 12/12 pods healthy."})
	if err != nil {
		return nil, err
	}

	planEnv, err := channelevents.BuildEnvelope("default", "demo-sess", channelevents.KindPlanUpdate, channelevents.PlanUpdatePayload{
		PlanName: "rollout",
		Items: []channelevents.PlanItemRef{
			{ID: "canary", Label: "Update the canary and watch error rate", Status: "done"},
			{ID: "roll", Label: "Roll the remaining 11 pods, 3 at a time", Status: "in_progress"},
			{ID: "smoke", Label: "Run the smoke suite against prod-us-east", Status: "pending"},
			{ID: "summary", Label: "Post a rollout summary to the thread", Status: "pending"},
		},
	})
	if err != nil {
		return nil, err
	}

	return []Specimen{
		{Name: "approval", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{approvalEnv}},
		{Name: "approval-resolved", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{approvalEnv, appliedEnv}, Pick: "/api/chat.update"},
		{Name: "join-approval", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{joinReqEnv}},
		{Name: "join-approval-resolved", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{joinReqEnv, joinAppliedEnv}, Pick: "/api/chat.update"},
		{Name: "plan-approval", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{planReqEnv}},
		{Name: "plan-approval-resolved", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{planReqEnv, planAppliedEnv}, Pick: "/api/chat.update"},
		{Name: "contact-share", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{contactReqEnv}},
		{Name: "contact-share-resolved", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{contactReqEnv, contactAppliedEnv}, Pick: "/api/chat.update"},
		{Name: "identity-choice", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{idChoiceReqEnv}},
		{Name: "identity-choice-dynamic", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{idChoiceDynReqEnv}},
		{Name: "info-leakage", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{infoLeakReqEnv}},
		{Name: "queued-messages", SubChannel: "interaction", Session: sess, Envelopes: []channelevents.Envelope{queuedReqEnv}},
		{Name: "message", SubChannel: "message", Session: sess, Envelopes: []channelevents.Envelope{msgEnv}},
		{Name: "plan", SubChannel: "message", Session: sess, Envelopes: []channelevents.Envelope{planEnv}},
	}, nil
}

// Generate captures every specimen and writes <Name>.json (pretty-printed Block
// Kit) into outDir. It fails if any specimen produces no blocks — a specimen
// that stops rendering blocks is the regression these fixtures exist to catch.
func Generate(outDir string) error {
	specs, err := Specimens()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	ctx := context.Background()
	for _, sp := range specs {
		posts, err := Capture(ctx, sp.SubChannel, sp.Session, sp.Envelopes...)
		if err != nil {
			return err
		}
		blocks := pickBlocks(posts, sp.Pick)
		if len(blocks) == 0 {
			return fmt.Errorf("blockcapture: specimen %q produced no blocks (%d posts)", sp.Name, len(posts))
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, blocks, "", "  "); err != nil {
			return fmt.Errorf("blockcapture: indent %q: %w", sp.Name, err)
		}
		pretty.WriteByte('\n')
		if err := os.WriteFile(filepath.Join(outDir, sp.Name+".json"), pretty.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}
