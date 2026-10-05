package goals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

const ExecutionResourceType = "agent_goal_execution"

func digest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// PrepareExecution fills all routing and class pins from authoritative objects,
// never from the model's arguments. Only a single-human route is supported.
func (s *Server) PrepareExecution(ctx context.Context, a domain.Actor, r *domain.ExecutionRequest) error {
	var sess v1.AgentSession
	ns, name, ok := strings.Cut(a.Session, "/")
	if !ok {
		return domain.ErrDenied
	}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return err
	}
	var class v1.AgentClass
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: a.Domain.Class}, &class); err != nil {
		return err
	}
	owner := identity.CanonicalFromTrusted(a.Domain.Owner, "verified goal owner")
	var catalog v1.UserIdentity
	if err := s.Reader.Get(ctx, client.ObjectKey{Name: useridentity.NameForSubject(owner.Subject())}, &catalog); err != nil {
		return err
	}
	r.Terms.OwnerCatalogUID = string(catalog.UID)
	hash, err := digest(class.Spec)
	if err != nil {
		return err
	}
	r.Terms.ClassDigest = hash
	r.Terms.SkillVersions = nil
	for _, skill := range class.Spec.Skills {
		r.Terms.SkillVersions = append(r.Terms.SkillVersions, skill.Ref)
	}
	binding := v1.OutboundBinding(&sess)
	if binding == nil {
		return fmt.Errorf("%w: private output channel required", domain.ErrDenied)
	}
	var channel v1.Channel
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: binding.Name}, &channel); err != nil {
		return err
	}
	pin, err := digest(binding)
	if err != nil {
		return err
	}
	r.Terms.Destination = domain.PrivateDestination{Channel: binding.Name, ChannelUID: string(channel.UID), Recipient: a.Domain.Owner, BindingDigest: pin}
	return nil
}

func (s *Server) Validate(ctx context.Context, g domain.Goal, t domain.ExecutionTerms) error {
	if s.Auth == nil || s.Reader == nil {
		return domain.ErrDenied
	}
	owner := identity.CanonicalFromTrusted(g.Domain.Owner, "verified goal owner")
	var user v1.UserIdentity
	if err := s.Reader.Get(ctx, client.ObjectKey{Name: useridentity.NameForSubject(owner.Subject())}, &user); err != nil {
		return fmt.Errorf("%w: owner catalog: %v", domain.ErrDenied, err)
	}
	if user.Spec.Subject != owner.Subject().String() || user.Spec.Suspended || user.UID == "" || string(user.UID) != t.OwnerCatalogUID || !user.DeletionTimestamp.IsZero() {
		return domain.ErrDenied
	}
	var class v1.AgentClass
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: g.Domain.Namespace, Name: g.Domain.Class}, &class); err != nil {
		return err
	}
	hash, err := digest(class.Spec)
	if err != nil {
		return err
	}
	if string(class.UID) != g.Domain.ClassUID || hash != t.ClassDigest || !class.DeletionTimestamp.IsZero() || class.Spec.IdentityMode != "userPassthrough" {
		return fmt.Errorf("%w: execution class changed or lacks user passthrough", domain.ErrDenied)
	}
	if len(class.Spec.ToolBundles) > 0 || len(class.Spec.MCPServers) > 0 || len(class.Spec.SidecarToolboxes) > 0 {
		return fmt.Errorf("%w: private reporting classes cannot attach external tool runtimes", domain.ErrDenied)
	}
	var skillVersions []string
	for _, skill := range class.Spec.Skills {
		skillVersions = append(skillVersions, skill.Ref)
	}
	if !reflect.DeepEqual(skillVersions, t.SkillVersions) {
		return domain.ErrDenied
	}
	// Skills must pin a commit; a mutable branch cannot be the reviewed version.
	for _, skill := range class.Spec.Skills {
		_, ref, ok := strings.Cut(skill.Ref, "@")
		decoded, err := hex.DecodeString(ref)
		if !ok || err != nil || len(decoded) != 20 {
			return fmt.Errorf("%w: execution skills must pin a commit", domain.ErrDenied)
		}
	}
	if err := s.executionPreference(ctx, &class, owner.String()); err != nil {
		return err
	}
	az := class.Spec.GetAuthz()
	if class.EffectiveSessionInteractPermission() != "" {
		return fmt.Errorf("%w: goal reports require a class without widened session access", domain.ErrDenied)
	}
	if az.InformationLeakage.ResolvedMode() != "enforcing" || (az.GetToolCalls().Mode != "" && az.GetToolCalls().Mode != "enforcing") || az.PlanGate == nil || az.PlanGate.Mode != "enforcing" || az.PlanGate.RequirePlan == nil || !*az.PlanGate.RequirePlan || az.PlanGate.Rendering == nil || az.PlanGate.Rendering.MaxAutoApproveHandles != 0 {
		return fmt.Errorf("%w: execution requires enforcing fresh plans and tool guards", domain.ErrDenied)
	}
	if permission := class.StartGatePermission(); permission != "" {
		allowed, err := s.Auth.CheckOnResource(ctx, "agentclass", g.Domain.Namespace+"/"+g.Domain.Class, permission, owner, true)
		if err != nil {
			return err
		}
		if !allowed {
			return domain.ErrDenied
		}
	}
	if err := s.ReadGoal(ctx, domain.Actor{Domain: g.Domain}, g); err != nil {
		return err
	}
	b := class.Spec.Budget
	if b != nil && ((b.MaxTurns > 0 && t.Bounds.Turns > int64(b.MaxTurns)) || (b.MaxTokens > 0 && t.Bounds.Tokens > b.MaxTokens) || (b.MaxDuration.Duration > 0 && time.Duration(t.Bounds.DurationSeconds)*time.Second > b.MaxDuration.Duration) || (b.SessionExpiration.Duration > 0 && time.Duration(t.Bounds.DurationSeconds)*time.Second > b.SessionExpiration.Duration)) {
		return fmt.Errorf("%w: reviewed bounds exceed class policy", domain.ErrInvalid)
	}
	if g.Execution == nil || g.Execution.SessionUID == "" {
		return domain.ErrDenied
	}
	ns, name, ok := strings.Cut(g.Execution.Session, "/")
	if !ok || ns != g.Domain.Namespace {
		return domain.ErrDenied
	}
	var source v1.AgentSession
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &source); err != nil {
		return err
	}
	if string(source.UID) != g.Execution.SessionUID || !source.DeletionTimestamp.IsZero() || source.Spec.Class != g.Domain.Class {
		return domain.ErrDenied
	}
	interact, err := s.Auth.CheckInteract(ctx, ns, name, owner, true)
	if err != nil {
		return err
	}
	if !interact {
		return domain.ErrDenied
	}
	binding := v1.OutboundBinding(&source)
	if binding == nil {
		return domain.ErrDenied
	}
	pin, err := digest(binding)
	if err != nil {
		return err
	}
	if binding.Name != t.Destination.Channel || pin != t.Destination.BindingDigest || t.Destination.Recipient != g.Domain.Owner {
		return domain.ErrDenied
	}
	var channel v1.Channel
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: binding.Name}, &channel); err != nil {
		return err
	}
	kind, registered := registry.Get(binding.Kind)
	if !registered {
		return domain.ErrDenied
	}
	resolver, ok := kind.(channelkinds.AudienceResolver)
	if !ok || !kind.DeliversToHuman() || resolver.AudienceCapability() != channelkinds.CapabilitySingleUser || channel.Spec.Kind != binding.Kind || string(channel.UID) != t.Destination.ChannelUID || !channel.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: private delivery route changed", domain.ErrDenied)
	}
	audience, err := resolver.ResolveAudience(ctx, channelkinds.SessionInfo{Namespace: ns, Name: name, SessionInitiator: owner, Channel: binding, Annotations: source.Annotations})
	if err != nil {
		return err
	}
	if len(audience) != 1 || audience[0] != owner.String() {
		return domain.ErrDenied
	}
	// First execution slice permits a private report only. Tools that can cause
	// external effects require the later result/idempotency contract.
	if len(t.AllowedOperations) != 1 || t.AllowedOperations[0] != "respond_to_user" {
		return fmt.Errorf("%w: supported execution operation is respond_to_user", domain.ErrInvalid)
	}
	return nil
}

func (s *Server) VerifyDecision(ctx context.Context, g domain.Goal, d domain.ExecutionDecision) error {
	var entry memory.Entry
	if err := json.Unmarshal([]byte(d.Witness), &entry); err != nil {
		return err
	}
	if entry.Kind != goalconsent.KindName || entry.Provenance == nil || entry.Provenance.Publisher != "system:channelsd" {
		return domain.ErrDenied
	}
	if err := provenance.VerifyEntrySignature(s.Keys, entry); err != nil {
		return err
	}
	var content goalconsent.Content
	if err := json.Unmarshal(entry.Content, &content); err != nil {
		return err
	}
	if g.Execution == nil || content.Approved == nil || *content.Approved != d.Approved || content.Owner != g.Domain.Owner || d.Owner != g.Domain.Owner || content.Goal.Execution == nil || content.Goal.Execution.Digest != g.Execution.Digest || d.Digest != g.Execution.Digest || content.Goal.Domain != g.Domain || content.Goal.ID != g.ID || !reflect.DeepEqual(content.Goal.Execution.Terms, g.Execution.Terms) || content.Goal.Execution.Session != g.Execution.Session || content.Goal.Execution.SessionUID != g.Execution.SessionUID {
		return domain.ErrDenied
	}
	var original memory.Entry
	if err := json.Unmarshal(content.RequestWitness, &original); err != nil {
		return err
	}
	if original.Provenance == nil || original.Provenance.Publisher != "system:operator" || original.Kind != goalconsent.KindName || original.Scope != entry.Scope || entry.Scope != (memory.Scope{Kind: "session", ID: g.Execution.Session}) {
		return domain.ErrDenied
	}
	if err := provenance.VerifyEntrySignature(s.Keys, original); err != nil {
		return err
	}
	var reviewed goalconsent.Content
	if err := json.Unmarshal(original.Content, &reviewed); err != nil {
		return err
	}
	if !reflect.DeepEqual(reviewed.Goal, content.Goal) || !reflect.DeepEqual(reviewed.Request, content.Request) {
		return domain.ErrDenied
	}
	var request channelevents.InteractionRequestPayload
	if err := json.Unmarshal(reviewed.Request, &request); err != nil {
		return err
	}
	if request.ExpiresAt == nil || !entry.CreatedAt.Before(*request.ExpiresAt) || entry.CreatedAt.Before(original.CreatedAt) || original.ID != "goalconsent-request-"+d.Digest || entry.ID != "goalconsent-decision-"+d.Digest || d.RequestID != entry.ID {
		return domain.ErrDenied
	}
	return nil
}
func (s *Server) AuthorizeDispatch(ctx context.Context, g domain.Goal) error {
	if s.Auth == nil || g.Execution == nil {
		return domain.ErrDenied
	}
	owner := identity.CanonicalFromTrusted(g.Domain.Owner, "verified goal owner")
	ok, err := s.Auth.CheckOnResource(ctx, ExecutionResourceType, g.Execution.Digest, "execute", owner, true)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrDenied
	}
	return nil
}

func (s *Server) decideHTTP(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || s.Tokens == nil || !s.Tokens.IsChannelsdToken(bearer) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if s.Service == nil || s.Keys == nil || s.Auth == nil {
		http.Error(w, "goal consent unavailable", 503)
		return
	}
	var entry memory.Entry
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 131072))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entry); err != nil {
		http.Error(w, "invalid decision", 400)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid decision", 400)
		return
	}
	var content goalconsent.Content
	if err := json.Unmarshal(entry.Content, &content); err != nil || content.Approved == nil || content.Goal.Execution == nil {
		http.Error(w, "invalid decision", 400)
		return
	}
	witness, err := json.Marshal(entry)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	decision := domain.ExecutionDecision{RequestID: entry.ID, Digest: content.Goal.Execution.Digest, Owner: content.Owner, Approved: *content.Approved, Witness: string(witness)}
	ctx := memory.WithCaller(memory.WithSystemApproval(r.Context(), "system:operator"), "system:operator")
	g, err := s.Service.DecideExecution(ctx, content.Goal.Domain, content.Goal.ID, decision)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if decision.Approved {
		err = s.Auth.Relations().WriteRelationships(ctx, []authz.Relation{
			{ResourceType: ExecutionResourceType, ResourceID: g.Execution.Digest, Relation: "domain", SubjectType: ResourceType, SubjectID: g.Domain.ID()},
			{ResourceType: ExecutionResourceType, ResourceID: g.Execution.Digest, Relation: "approved", SubjectType: "user", SubjectID: g.Domain.Owner, ExpiresAt: g.Execution.Terms.ExpiresAt},
		})
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// A class may expose the existing confirmed-preference surface as an execution
// opt-out. Invalid or unset values fail closed whenever the schema declares it.
func (s *Server) executionPreference(ctx context.Context, class *v1.AgentClass, owner string) error {
	const key = "goal_execution_enabled"
	var schema []v1.UserPreferenceSchema
	for _, entry := range class.Spec.UserPreferences {
		if entry.Name == key {
			if entry.Type != "bool" {
				return domain.ErrDenied
			}
			schema = append(schema, entry)
		}
	}
	if len(schema) == 0 {
		return nil
	}
	scope, err := memory.UserScope(owner)
	if err != nil {
		return err
	}
	rows, err := s.Memory.Query(ctx, memory.Query{Scope: scope, Kinds: []string{userpreference.KindName}, IDs: []string{userpreference.EntryID(class.Namespace, class.Name, key)}, Limit: 1})
	if err != nil {
		return err
	}
	values := map[string]apiextv1.JSON{}
	for _, entry := range rows.Entries {
		var preference userpreference.Preference
		if err := json.Unmarshal(entry.Content, &preference); err != nil {
			return err
		}
		values[key] = apiextv1.JSON{Raw: preference.Value}
	}
	var settings v1.AgentSettings
	globals := map[string]v1.PreferenceGlobal{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: class.Namespace, Name: v1.AgentSettingsName}, &settings); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if err == nil {
		globals = settings.Spec.ClassUserPreferences[class.Name]
	}
	snapshot := preferences.Resolve(schema, globals, values)
	if len(snapshot.Violations) > 0 || len(snapshot.Keys) != 1 || snapshot.Keys[0].Value == nil {
		return domain.ErrDenied
	}
	var enabled bool
	if err := json.Unmarshal(snapshot.Keys[0].Value.Raw, &enabled); err != nil {
		return err
	}
	if !enabled {
		return domain.ErrDenied
	}
	return nil
}
