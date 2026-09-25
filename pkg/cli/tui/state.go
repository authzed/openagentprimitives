package tui

import "slices"

// Note is one line of the post-run summary: what was decided, in the user's
// terms. Screens record these; summary.go renders them once the run is over,
// so scrollback holds a readable record of it.
type Note struct {
	Label string
	Value string
}

// State carries answers across screens and accumulates the summary.
//
// Seeding State from flags BEFORE Run is what makes non-interactive mode
// work: a screen whose answer is already present returns a nil group from
// Prepare and asks nothing.
//
// Not safe for concurrent use — the sequencer drives screens serially.
type State struct {
	strs  map[string]string
	bools map[string]bool
	lists map[string][]string
	notes []Note
}

// NewState returns an empty State.
func NewState() *State {
	return &State{
		strs:  map[string]string{},
		bools: map[string]bool{},
		lists: map[string][]string{},
	}
}

// Set records a string answer.
func (s *State) Set(key, val string) { s.strs[key] = val }

// Get returns a string answer, or "" when absent.
func (s *State) Get(key string) string { return s.strs[key] }

// SetBool records a boolean answer.
func (s *State) SetBool(key string, v bool) { s.bools[key] = v }

// Bool returns a boolean answer, or false when absent.
func (s *State) Bool(key string) bool { return s.bools[key] }

// SetAll records a multi-value answer. The slice is cloned so a later mutation
// by the caller cannot corrupt recorded state.
func (s *State) SetAll(key string, vals []string) { s.lists[key] = slices.Clone(vals) }

// All returns a multi-value answer, or nil when absent. The result is a clone.
func (s *State) All(key string) []string {
	v, ok := s.lists[key]
	if !ok {
		return nil
	}
	return slices.Clone(v)
}

// Has reports whether key was answered, in any of the three shapes. An
// explicitly-empty string counts as answered — "answered with nothing" and
// "never asked" must stay distinguishable, because only the latter is a
// fail-closed error in non-interactive mode.
func (s *State) Has(key string) bool {
	if _, ok := s.strs[key]; ok {
		return true
	}
	if _, ok := s.bools[key]; ok {
		return true
	}
	_, ok := s.lists[key]
	return ok
}

// Note appends a summary line.
func (s *State) Note(label, value string) {
	s.notes = append(s.notes, Note{Label: label, Value: value})
}

// Notes returns the summary lines in the order they were recorded.
func (s *State) Notes() []Note { return slices.Clone(s.notes) }

// KeyNonInteractive records whether the run has a human at the terminal. Set by
// RunWith from Options.NonInteractive, never by a screen and never by a flag —
// the dispatcher's --answer check rejects keys no screen declares, and RunWith
// overwrites the key regardless.
//
// It exists because a screen sees only ctx and State, while some steps behave
// differently with nobody watching: a step that would otherwise record a
// failure and let a later screen offer a way out must instead return that
// failure, because there is no later screen anybody will read.
const KeyNonInteractive = "tui.non-interactive"

// NonInteractive reports whether the run was told not to prompt.
func (s *State) NonInteractive() bool { return s.Bool(KeyNonInteractive) }
