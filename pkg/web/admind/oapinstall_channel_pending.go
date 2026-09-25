// The state that spans the two requests a declared channel takes to set up:
// the one that renders the form, and the one that submits it.
//
// WHY THERE IS SERVER-SIDE STATE AT ALL, when every other admind route is a
// pure function of its request. Two facts about a declared channel are the
// BUNDLE's and must not be re-supplied by the browser: the Channel's name, and
// the AgentClass it binds to. B-R13 is the rule — a seeded answer is omitted
// from the form, never offered as an editable default — and a form field is
// exactly how it would come back: an operator who edits the declared name gets
// a Channel whose "<name>-creds" Secret the bundled AgentIdentity cannot find,
// which is the defect channelplan.LintRequiredChannels exists to catch and
// which no lint can see, because the lint reads the bundle and the edit is in a
// form. Holding the declaration server-side and handing the browser an opaque
// token means the answer never has a field to arrive in.
//
// The handoff needs the same store for a different reason — see
// pendingChannelSetup.handoff — which is why the two live together rather than
// each growing their own.
package admind

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

const (
	// channelSetupTTL bounds how long a pending setup stays usable.
	//
	// Long enough for the slowest legitimate path — an operator reading the
	// form, going to an external service to create an App, and coming back —
	// and short enough that an abandoned install's token is not a standing
	// capability. It is the lifetime of a CSRF nonce as much as of a form:
	// the handoff's `state` lives on the same record and expires with it.
	channelSetupTTL = 30 * time.Minute

	// maxPendingChannelSetups bounds the store.
	//
	// Every entry is created by a caller who already holds platform
	// install_agent, so this is not a public-facing flood gate; it is the
	// difference between a UI that re-renders a form in a loop costing
	// bounded memory and costing all of it. Expired entries are swept before
	// the bound is consulted, so reaching it means that many LIVE setups —
	// far more than any real install produces.
	maxPendingChannelSetups = 128
)

// pendingChannelSetup is one declared channel this admind has offered a form
// for, held between the request that rendered it and the request(s) that
// complete it.
type pendingChannelSetup struct {
	// token is the opaque handle the browser carries. It is a capability:
	// holding it is what lets a request act on this declaration, so it is
	// randomly generated and never derived from anything guessable.
	token string
	// owner is the subject the form was rendered for. EVERY later use is
	// checked against it, so one operator's pending install cannot be driven
	// by another — both of them hold install_agent, which makes the platform
	// permission check alone insufficient to tell them apart.
	owner identity.CanonicalUserID

	namespace string
	// agentClass is the name the AgentClass CR has in the cluster — the
	// installed name, resolved from the bundle, not anything the browser said.
	agentClass string
	// required is the declaration verbatim: kind, name, role, purpose.
	required oap.RequiredChannel
	// seeded is what the plan answered on the operator's behalf, keyed the way
	// a question names it. It is the half of the answer map the browser may
	// not write; see the package doc.
	seeded map[string]string
	// notSeeded is why a value this install TRIED to pre-fill is missing, in
	// the planner's own prose. Kept because one of those absences is fatal to
	// the handoff rather than merely inconvenient: with no external base URL
	// there is no callback to send the operator back to, and the operator is
	// owed the reason the cluster has none rather than a generic refusal.
	notSeeded map[string]string

	createdAt time.Time

	// handoff is the LIVE *channelkinds.HandoffSpec a begun round trip is
	// running against — never a serialized copy, because it CANNOT be one:
	// Begin, Complete and FallbackGuidance are Go closures over state the kind
	// captured when it described the handoff (the namespace, the working
	// directory, the conversion client). Begin runs in one HTTP request and
	// Complete in another, so the spec has to survive between them in memory.
	//
	// nil until a handoff is begun, and left in place afterwards: the submit
	// request that answers the fallback questions replays the exchange through
	// this same spec, so wizardrun.Finish still sees a handoff in its proper
	// position rather than answers that appeared from nowhere.
	handoff *channelkinds.HandoffSpec
	// handoffIn is the WizardInput the spec was described with, kept so the
	// submit request finishes against the same one rather than a second one
	// built from a re-read cluster.
	handoffIn channelkinds.WizardInput
	// handoffAnswers is what Begin was called with. The operator does not
	// re-send them on the submit request, and losing them would drop values
	// Result needs (github's organization, for one) that the browser only ever
	// supplied once.
	handoffAnswers map[string]string
	// handoffState is this round trip's CSRF nonce: generated here, put on the
	// address the operator is sent to, and required back on the callback
	// BEFORE anything is read out of it. Empty when no handoff is in flight,
	// and an empty one never matches — see byHandoffState.
	handoffState string
	// handoffDerived is what Complete exchanged the callback for. It holds the
	// App's private key and webhook secret for a github handoff, so it lives
	// here and reaches no response body.
	handoffDerived map[string]string

	graphBinding *channelSetupBinding
	graphState   graphSetupState
	staged       stagedChannelSetup
	cleanup      *channelSetupCleanup
	cancelExpiry func()
	redactor     *channelSetupRedactor
}

type channelSetupRedactor struct {
	mu sync.Mutex
	r  *redact.Redactor
}

type redactedChannelError struct{ message string }

func (e *redactedChannelError) Error() string { return e.message }

func newChannelSetupRedactor() *channelSetupRedactor {
	return &channelSetupRedactor{r: redact.New()}
}

func (r *channelSetupRedactor) register(values map[string]string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, value := range values {
		r.r.RegisterSensitive(value, redact.Descriptor{Description: "channel setup answer", Name: name})
	}
}

func (r *channelSetupRedactor) registerValues(values []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range values {
		r.r.RegisterSensitive(value, redact.Descriptor{Description: "channel setup answer"})
	}
}

func (r *channelSetupRedactor) string(value string) string {
	if r == nil {
		return value
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.RedactInString(value)
}

func (r *channelSetupRedactor) err(err error) error {
	if err == nil {
		return nil
	}
	// Never retain/unwrap a provider or cleanup cause: it may contain the
	// plaintext value this token-local redactor removed.
	return &redactedChannelError{message: r.string(err.Error())}
}

func redactStrings(r *channelSetupRedactor, values []string) []string {
	redacted := slices.Clone(values)
	for i := range redacted {
		redacted[i] = r.string(redacted[i])
	}
	return redacted
}

type channelSetupBinding struct {
	RootDigest       string
	RootNamespace    string
	RootInstall      string
	AgentPath        string
	AgentClass       string
	CredentialSecret string
	Required         oap.RequiredChannel
}

type graphSetupState uint8

const (
	graphSetupPending graphSetupState = iota
	graphSetupHandoffBeginning
	graphSetupHandoffAwaiting
	graphSetupHandoffCompleting
	graphSetupHandoffResolved
	graphSetupResolving
	graphSetupComplete
)

type stagedChannelSetup struct {
	output               channelkinds.WizardOutput
	sensitiveValues      []string
	rollbackPrerequisite func(context.Context) error
}

type channelSetupCleanup struct {
	once sync.Once
	err  error
	fn   func(context.Context) error
}

func (c *channelSetupCleanup) run(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		if c.fn != nil {
			c.err = c.fn(ctx)
		}
	})
	return c.err
}

type claimedChannelSetup struct {
	mu              sync.Mutex
	binding         channelSetupBinding
	staged          stagedChannelSetup
	cleanup         *channelSetupCleanup
	rollbackApplied func(context.Context) error
	redactor        *channelSetupRedactor
	finished        bool
}

func (c *claimedChannelSetup) invalidate(ctx context.Context) error {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return c.redactor.err(c.cleanup.run(ctx))
	}
	c.finished = true
	c.mu.Unlock()
	return c.redactor.err(c.cleanup.run(ctx))
}

func (c *claimedChannelSetup) rollbackApply(ctx context.Context) error {
	c.mu.Lock()
	rollback := c.rollbackApplied
	c.mu.Unlock()
	if rollback == nil {
		return nil
	}
	return c.redactor.err(rollback(ctx))
}

func (c *claimedChannelSetup) setApplyRollback(rollback func(context.Context) error) {
	c.mu.Lock()
	c.rollbackApplied = rollback
	c.mu.Unlock()
}

func (c *claimedChannelSetup) finalize() {
	c.mu.Lock()
	c.rollbackApplied = nil
	c.cleanup = nil
	c.mu.Unlock()
}

func (c *claimedChannelSetup) consume() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return errors.New("channel setup claim is already finished")
	}
	c.finished = true
	return nil
}

// channelSetupStore holds the pending setups. Process-local and deliberately
// so: a token is a short-lived handle to a form, and an operator whose admind
// restarted mid-install re-renders the form rather than resuming one whose
// server-side half is gone. The one thing that outlives a restart badly is a
// handoff whose external App was already created — see handleChannelHandoff's
// own note.
type channelSetupStore struct {
	mu      sync.Mutex
	byToken map[string]*pendingChannelSetup
	// now is the clock, injectable so a test can expire an entry without
	// sleeping for half an hour.
	now      func() time.Time
	ttl      time.Duration
	max      int
	schedule func(time.Duration, func()) func()
	// onCleanupError makes asynchronous expiry/sweep failures observable.
	// Synchronous invalidation returns its error to the workflow instead.
	onCleanupError func(error)
}

func newChannelSetupStore() *channelSetupStore {
	return &channelSetupStore{
		byToken: map[string]*pendingChannelSetup{},
		now:     time.Now,
		ttl:     channelSetupTTL,
		max:     maxPendingChannelSetups,
		schedule: func(after time.Duration, fn func()) func() {
			timer := time.AfterFunc(after, fn)
			return func() { timer.Stop() }
		},
		onCleanupError: func(err error) {
			log.Printf("admind: expired channel prerequisite cleanup failed: %v", err)
		},
	}
}

func (s *channelSetupStore) reportCleanupError(err error) {
	if err != nil && s.onCleanupError != nil {
		s.onCleanupError(err)
	}
}

// errNoPendingSetup is what a caller gets for a token that never existed, has
// expired, or belongs to somebody else.
//
// ONE error for all three, deliberately. Distinguishing them tells whoever is
// holding a token they should not have which of those three it is, and the
// only actionable answer is the same in every case: render the form again.
var errNoPendingSetup = fmt.Errorf("no such pending channel setup, or it has expired; re-open the install form to get a new one")

// errTooManyPendingSetups is returned rather than evicting somebody else's
// live setup. Eviction under pressure would let one operator's form-rendering
// loop silently invalidate another's in-flight handoff, which is worse than a
// loud refusal on a bound no real install approaches.
var errTooManyPendingSetups = fmt.Errorf("too many channel setups are pending on this operator; finish or abandon one and try again")

var errPendingSetupIncomplete = fmt.Errorf("channel setup is not complete; finish its required questions or browser handoff first")

// put stores a setup and returns its token.
//
// A setup matching an EXISTING live entry — same owner, same namespace, same
// AgentClass, same declaration — reuses that entry's token instead of minting
// a second. The install form is routinely re-rendered (every 400 the operator
// corrects a manifest answer on is another render), and a token per render
// would grow the store without any of them being a distinct thing to complete.
//
// The bound is PER OWNER, which is what its refusal has always claimed. A
// global one lets a single operator's render loop deny every other operator for
// the length of a TTL, with a message pointing them at setups that are not
// theirs — an availability bug wearing a misleading sentence. maxPendingSetups
// is a ceiling on ONE admin's in-flight work; the number of admins is a
// different quantity and is bounded by who holds install_agent.
func (s *channelSetupStore) put(p *pendingChannelSetup) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if p.redactor == nil {
		p.redactor = newChannelSetupRedactor()
	}
	p.redactor.register(p.seeded)

	mine := 0
	for _, existing := range s.byToken {
		if existing.owner != p.owner {
			continue
		}
		mine++
		if samePendingChannelSetup(existing, p) {
			return existing.token, nil
		}
	}
	if mine >= s.max {
		return "", errTooManyPendingSetups
	}

	tok, err := newSetupToken()
	if err != nil {
		return "", err
	}
	p.token = tok
	p.createdAt = s.now()
	s.byToken[tok] = p
	if s.schedule != nil {
		p.cancelExpiry = s.schedule(s.ttl, func() { s.expire(tok) })
	}
	return tok, nil
}

func samePendingChannelSetup(existing, proposed *pendingChannelSetup) bool {
	if existing == nil || proposed == nil {
		return false
	}
	if existing.graphBinding != nil || proposed.graphBinding != nil {
		return existing.graphBinding != nil && proposed.graphBinding != nil && *existing.graphBinding == *proposed.graphBinding && existing.owner == proposed.owner
	}
	return existing.namespace == proposed.namespace && existing.agentClass == proposed.agentClass &&
		existing.required.Kind == proposed.required.Kind && existing.required.Name == proposed.required.Name
}

func (s *channelSetupStore) completeGraph(token string, owner identity.CanonicalUserID, binding channelSetupBinding, staged stagedChannelSetup) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil || *p.graphBinding != binding {
		return errNoPendingSetup
	}
	if p.graphState != graphSetupResolving {
		return errors.New("channel setup is not held by the active resolver")
	}
	p.staged = cloneStagedChannelSetup(staged)
	p.redactor.registerValues(staged.sensitiveValues)
	p.cleanup = &channelSetupCleanup{fn: staged.rollbackPrerequisite}
	p.graphState = graphSetupComplete
	return nil
}

// beginGraphResolve atomically makes one submit request the sole resolver.
// The claim happens before any wizard/provider callback so concurrent submits
// cannot both spend credentials or create upstream state.
func (s *channelSetupStore) beginGraphResolve(token string, owner identity.CanonicalUserID) (pendingChannelSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil {
		return pendingChannelSetup{}, errNoPendingSetup
	}
	if p.graphState != graphSetupPending && p.graphState != graphSetupHandoffResolved {
		return pendingChannelSetup{}, errors.New("channel setup is already resolving or complete; re-open the install form to get a new one")
	}
	p.graphState = graphSetupResolving
	return copyPending(p), nil
}

// beginGraphHandoff exclusively claims a graph token and stores its nonce in
// the same critical section. It runs before any provider Skipped/Begin callback
// so neither a second begin nor an ordinary submit can enter provider work.
func (s *channelSetupStore) beginGraphHandoff(token string, owner identity.CanonicalUserID, nonce string) (pendingChannelSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil {
		return pendingChannelSetup{}, errNoPendingSetup
	}
	if p.graphState != graphSetupPending {
		return pendingChannelSetup{}, errors.New("channel setup is already resolving or complete; re-open the install form to get a new one")
	}
	p.graphState = graphSetupHandoffBeginning
	p.handoffState = nonce
	return copyPending(p), nil
}

func (s *channelSetupStore) completeGraphHandoffBegin(token string, owner identity.CanonicalUserID, nonce string, handoff *channelkinds.HandoffSpec, in channelkinds.WizardInput, answers map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil || p.graphState != graphSetupHandoffBeginning || subtle.ConstantTimeCompare([]byte(p.handoffState), []byte(nonce)) != 1 {
		return errNoPendingSetup
	}
	p.handoff = handoff
	p.handoffIn = in
	p.handoffAnswers = maps.Clone(answers)
	p.handoffDerived = nil
	p.graphState = graphSetupHandoffAwaiting
	return nil
}

func (s *channelSetupStore) releaseGraphHandoffBegin(token string, owner identity.CanonicalUserID, nonce string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil || p.graphState != graphSetupHandoffBeginning || subtle.ConstantTimeCompare([]byte(p.handoffState), []byte(nonce)) != 1 {
		return errNoPendingSetup
	}
	p.handoffState = ""
	p.graphState = graphSetupPending
	return nil
}

func (s *channelSetupStore) failGraphHandoff(ctx context.Context, token string, owner identity.CanonicalUserID, nonce string, phases ...graphSetupState) error {
	s.mu.Lock()
	p, ok := s.byToken[token]
	allowed := false
	for _, phase := range phases {
		allowed = allowed || ok && p.graphState == phase
	}
	if !ok || p.owner != owner || p.graphBinding == nil || !allowed || subtle.ConstantTimeCompare([]byte(p.handoffState), []byte(nonce)) != 1 {
		s.mu.Unlock()
		return nil
	}
	delete(s.byToken, token)
	if p.cancelExpiry != nil {
		p.cancelExpiry()
	}
	s.mu.Unlock()
	return p.redactor.err(p.cleanup.run(ctx))
}

func (s *channelSetupStore) completeGraphHandoff(token string, owner identity.CanonicalUserID, nonce string, derived map[string]string) (pendingChannelSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil || p.graphState != graphSetupHandoffCompleting || subtle.ConstantTimeCompare([]byte(p.handoffState), []byte(nonce)) != 1 {
		return pendingChannelSetup{}, errNoPendingSetup
	}
	p.handoffState = ""
	p.handoffDerived = maps.Clone(derived)
	if p.handoffAnswers == nil {
		p.handoffAnswers = map[string]string{}
	}
	for k, v := range derived {
		p.handoffAnswers[k] = v
	}
	p.graphState = graphSetupHandoffResolved
	return copyPending(p), nil
}

func (s *channelSetupStore) failGraphResolve(ctx context.Context, token string, owner identity.CanonicalUserID) error {
	s.mu.Lock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphState != graphSetupResolving {
		s.mu.Unlock()
		return nil
	}
	delete(s.byToken, token)
	if p.cancelExpiry != nil {
		p.cancelExpiry()
	}
	s.mu.Unlock()
	return p.redactor.err(p.cleanup.run(ctx))
}

// releaseGraphResolve makes a validation-only missing-input response retryable
// without reopening provider work. The resolver claim is still exclusive; the
// exact held state is required, and sealed handoff output selects the only
// legal state to return to.
func (s *channelSetupStore) releaseGraphResolve(token string, owner identity.CanonicalUserID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil {
		return errNoPendingSetup
	}
	if p.graphState != graphSetupResolving {
		return errors.New("channel setup is not held by the active resolver")
	}
	p.graphState = graphSetupPending
	if p.handoffDerived != nil {
		p.graphState = graphSetupHandoffResolved
	}
	return nil
}

func cloneStagedChannelSetup(staged stagedChannelSetup) stagedChannelSetup {
	cloned := staged
	if staged.output.SecretManifest != nil {
		cloned.output.SecretManifest = staged.output.SecretManifest.DeepCopy()
	}
	if staged.output.ChannelManifest != nil {
		cloned.output.ChannelManifest = staged.output.ChannelManifest.DeepCopy()
	}
	if staged.output.CapabilityPatch != nil {
		cloned.output.CapabilityPatch = staged.output.CapabilityPatch.DeepCopy()
	}
	cloned.output.Notes = slices.Clone(staged.output.Notes)
	cloned.output.Summary = slices.Clone(staged.output.Summary)
	cloned.sensitiveValues = slices.Clone(staged.sensitiveValues)
	return cloned
}

func (s *channelSetupStore) graphReady(token string, owner identity.CanonicalUserID, binding channelSetupBinding) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil || *p.graphBinding != binding {
		return false, errNoPendingSetup
	}
	return p.graphState == graphSetupComplete, nil
}

func (s *channelSetupStore) claimGraph(token string, owner identity.CanonicalUserID, binding channelSetupBinding) (*claimedChannelSetup, error) {
	s.mu.Lock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner || p.graphBinding == nil || *p.graphBinding != binding {
		s.mu.Unlock()
		return nil, errNoPendingSetup
	}
	if p.graphState != graphSetupComplete {
		s.mu.Unlock()
		return nil, errPendingSetupIncomplete
	}
	delete(s.byToken, token)
	if p.cancelExpiry != nil {
		p.cancelExpiry()
	}
	s.mu.Unlock()
	return &claimedChannelSetup{binding: binding, staged: p.staged, cleanup: p.cleanup, redactor: p.redactor}, nil
}

func (s *channelSetupStore) expire(token string) {
	s.mu.Lock()
	p, ok := s.byToken[token]
	if ok {
		delete(s.byToken, token)
	}
	s.mu.Unlock()
	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.reportCleanupError(p.redactor.err(p.cleanup.run(ctx)))
	}
}

// get returns the setup a token names, provided the asking subject owns it.
//
// BY VALUE, with every map deep-copied. The store owns the only
// *pendingChannelSetup there is, and it is reachable from two concurrent
// requests — a begin and a callback, or two callbacks a browser prefetch
// produced. Handing out the pointer made every field read and every field
// write a race on state the store's own mutex was meanwhile guarding, and one
// arm of it was fatal rather than merely wrong: two goroutines merging into
// handoffAnswers is `concurrent map writes`, which is unrecoverable and takes
// the operator process with it. A caller that needs to CHANGE something calls
// update.
func (s *channelSetupStore) get(token string, owner identity.CanonicalUserID) (pendingChannelSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	p, ok := s.byToken[token]
	if !ok || p.owner != owner {
		return pendingChannelSetup{}, errNoPendingSetup
	}
	return copyPending(p), nil
}

// update applies fn to the stored setup under the store's lock, and returns
// the result by value.
//
// fn runs holding the mutex, so it MUST NOT do I/O or call back into the
// store: it exists to move already-computed values into the record, not to be
// the place work happens. Every mutation of a pending setup goes through here.
func (s *channelSetupStore) update(token string, owner identity.CanonicalUserID, fn func(*pendingChannelSetup)) (pendingChannelSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	p, ok := s.byToken[token]
	if !ok || p.owner != owner {
		return pendingChannelSetup{}, errNoPendingSetup
	}
	fn(p)
	return copyPending(p), nil
}

// claimHandoffState finds the setup whose in-flight round trip carries this
// `state`, CLEARS the nonce, and returns the setup — all under one lock.
//
// CHECK AND CLEAR ARE ONE OPERATION, and that is the whole reason this is a
// store method rather than a lookup the handler follows with an assignment.
// Split in two, a browser that delivers the callback twice — a prefetch plus
// the navigation the operator actually made — has both requests match before
// either clears, and BOTH go on to spend the code at the external service.
// Single-use has to be enforced by construction; a comment saying "cleared
// before the exchange" is not enforcement.
//
// LOOKED UP BY STATE rather than by token because the external service echoes
// only what the client put on the address it sent the operator to, and the
// setup token is not that: putting the token on the redirect would publish a
// standing capability into a browser's history, a referrer header, and the
// service's own logs. The token never leaves the UI.
//
// The comparison is CONSTANT-TIME, and the scan is linear for that reason. A
// map lookup would compare with memequal and return at the first differing
// byte, which leaks how much of the nonce a guess got right to an attacker who
// can retry; the store holds at most `max` entries per owner, so scanning them
// all costs nothing. An empty stored state never matches, so a setup with no
// handoff in flight is not selectable by a caller sending `state=`.
func (s *channelSetupStore) claimHandoffState(state string, owner identity.CanonicalUserID) (pendingChannelSetup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	if state == "" {
		return pendingChannelSetup{}, errNoPendingSetup
	}
	var found *pendingChannelSetup
	for _, p := range s.byToken {
		if p.handoffState == "" || (p.graphBinding != nil && p.graphState != graphSetupHandoffAwaiting) {
			continue
		}
		// Every candidate is compared, with no early exit, so the work done
		// does not depend on which entry matched.
		if subtle.ConstantTimeCompare([]byte(p.handoffState), []byte(state)) == 1 && p.owner == owner {
			found = p
		}
	}
	if found == nil {
		return pendingChannelSetup{}, errNoPendingSetup
	}
	// Spent, before anything else can look. Graph setups retain the nonce only
	// as an exact Complete claim and move to a non-matchable phase; direct
	// setups retain their historical clear-on-claim behavior.
	if found.graphBinding != nil {
		found.graphState = graphSetupHandoffCompleting
	} else {
		found.handoffState = ""
	}
	return copyPending(found), nil
}

// copyPending is a value copy with every map duplicated, so nothing a caller
// does to what it reads can reach the record the store still owns.
//
// handoff is a POINTER and is deliberately shared: a *channelkinds.HandoffSpec
// is three closures over state the kind captured when it described the
// handoff, it is never written after Begin stores it, and a copy of it would
// not be a copy of the closures anyway.
func copyPending(p *pendingChannelSetup) pendingChannelSetup {
	out := *p
	out.seeded = maps.Clone(p.seeded)
	out.notSeeded = maps.Clone(p.notSeeded)
	out.handoffAnswers = maps.Clone(p.handoffAnswers)
	out.handoffDerived = maps.Clone(p.handoffDerived)
	return out
}

// forget drops a setup that has been completed. A setup that FAILED is left in
// place: the operator corrects an answer and submits the same form again, and
// making them re-render it would lose whatever a completed handoff already
// produced.
func (s *channelSetupStore) forget(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.byToken[token]; p != nil && p.cancelExpiry != nil {
		p.cancelExpiry()
	}
	delete(s.byToken, token)
}

// abandon invalidates a graph setup that became unnecessary before claim and
// cleans any prerequisite created while staging it. Direct setup records have
// no staged prerequisite and continue to use forget after successful apply.
func (s *channelSetupStore) abandon(ctx context.Context, token string, owner identity.CanonicalUserID) error {
	s.mu.Lock()
	p, ok := s.byToken[token]
	if !ok || p.owner != owner {
		s.mu.Unlock()
		return errNoPendingSetup
	}
	delete(s.byToken, token)
	if p.cancelExpiry != nil {
		p.cancelExpiry()
	}
	s.mu.Unlock()
	return p.redactor.err(p.cleanup.run(ctx))
}

// sweepLocked drops everything past its TTL on access as a backstop to each
// entry's scheduled expiry. The scheduled path guarantees abandoned receipts
// are cleaned even when no later request touches this process; the sweep keeps
// injected clocks and delayed timers from retaining an already-dead token.
func (s *channelSetupStore) sweepLocked() {
	cutoff := s.now().Add(-s.ttl)
	for tok, p := range s.byToken {
		if p.createdAt.Before(cutoff) {
			delete(s.byToken, tok)
			if p.cancelExpiry != nil {
				p.cancelExpiry()
			}
			go func(cleanup *channelSetupCleanup, redactor *channelSetupRedactor) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				s.reportCleanupError(redactor.err(cleanup.run(ctx)))
			}(p.cleanup, p.redactor)
		}
	}
}

// newSetupToken mints an unguessable handle. 32 bytes from crypto/rand,
// base64url so it survives a query string and a JSON body unescaped.
func newSetupToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate a channel setup token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
