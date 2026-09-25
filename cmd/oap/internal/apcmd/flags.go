package apcmd

import (
	"fmt"
	"io"
	"math"
	"time"

	"github.com/spf13/cobra"
)

// The flags below are registered by more than one command. Each is declared
// exactly once here so its name, shorthand, default and help text cannot
// drift: a flag whose default differs between two commands, or whose shorthand
// exists on one and not the other, is invisible at build time and shows up
// only when a user runs the second command the way they learned on the first.
//
// A flag that merely SHARES A NAME does not belong here. `--dry-run` names a
// mode on `oap install` and `oap clean` (DryRunModeFlag below) and is a bool on
// `oap settings apply`, whose preview is client-side only and has no second
// mode to name; `oap pin`'s `--kind` selects a pinning kind and
// `oap memory put`'s names the single kind being written, neither of which is
// the memory-kind filter below; `oap artifact store list`'s `--limit` counts 0
// as unlimited. Those are different flags that happen to be spelled alike, and
// forcing them onto one registration would erase the difference rather than
// centralize it.

// AllNamespacesFlag registers the -A/--all-namespaces list-scope flag.
func AllNamespacesFlag(cmd *cobra.Command, target *bool) {
	cmd.Flags().BoolVarP(target, "all-namespaces", "A", false, "List across all namespaces")
}

// FollowTimeout is the client-side ceiling on a --follow stream. It is a
// property of the pair, not of either command, so it lives with the flag that
// implies it.
const FollowTimeout = 30 * time.Minute

// FollowFlags registers the -f/--follow + --timeout pair the log-streaming
// commands share. They are registered together because --timeout means
// something only while --follow is set: splitting them is how one command ends
// up able to follow forever.
//
// -f is --follow only on the streaming commands; on the commands that read a
// document it is --file (see FileFlag). No command does both.
func FollowFlags(cmd *cobra.Command, follow *bool, timeout *time.Duration) {
	cmd.Flags().BoolVarP(follow, "follow", "f", false, "Keep streaming until the session is terminal or canceled")
	cmd.Flags().DurationVar(timeout, "timeout", FollowTimeout, "Client-side cap when --follow is set")
}

// SessionNameFlag registers --name for the commands that start a session.
func SessionNameFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "name", "", "Session name (default: <agent>-<short-uuid>)")
}

// JSONFlag registers --json, the machine-readable alternative to a command's
// rendered output.
//
// usage stays per-command on purpose: it names the document THIS command emits
// (an array, a single object, a per-publisher report), which is the one thing
// about the flag that is not shared. The name and the default are.
func JSONFlag(cmd *cobra.Command, target *bool, usage string) {
	cmd.Flags().BoolVar(target, "json", false, usage)
}

// OutputFileFlag registers -o/--output: the file a command writes its bytes to
// instead of stdout. Empty means stdout, which is why the commands that
// default to writing a derived filename (`oap agent package`, `oap agent pull`)
// register their own -o/--output with the usage that documents that default —
// there, empty does not mean stdout.
//
// usagePrefix is for a command that honours the flag on only one of its paths;
// it is empty where --output always applies.
func OutputFileFlag(cmd *cobra.Command, target *string, usagePrefix string) {
	cmd.Flags().StringVarP(target, "output", "o", "", usagePrefix+"Write output to this file instead of stdout")
}

// FileFlag registers -f/--file, the path a command reads its input document
// from.
//
// usage stays per-command because it names the document THIS command expects
// (a toolspec, a YAML manifest, a memory entry) and whether '-' means stdin.
// The name, shorthand and default are shared.
func FileFlag(cmd *cobra.Command, target *string, usage string) {
	cmd.Flags().StringVarP(target, "file", "f", "", usage)
}

// TimeRangeFlags registers the --since/--until pair that bounds a read to a
// window. Registered together because a half-open window is still a window:
// a command that accepted one and not the other would be answering a question
// the user did not ask.
func TimeRangeFlags(cmd *cobra.Command, since, until *string) {
	cmd.Flags().StringVar(since, "since", "", "Time filter: RFC3339 or relative duration (5m, 1h)")
	cmd.Flags().StringVar(until, "until", "", "Time filter: RFC3339 or relative duration (5m, 1h)")
}

// MemoryKindFilterFlag registers --kind, the comma-separated memory-kind
// filter the memory read commands share. It is a filter over many kinds;
// `oap memory put --kind` names the one kind being written and is a different
// flag.
func MemoryKindFilterFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "kind", "", "Comma-separated kind filter (e.g. turn,label)")
}

// MemoryTagFilterFlag registers --tag, the repeatable tag filter the memory
// read commands share.
func MemoryTagFilterFlag(cmd *cobra.Command, target *[]string) {
	cmd.Flags().StringSliceVar(target, "tag", nil, "Tag filter (repeatable, AND semantics)")
}

// MemoryFieldFilterFlag registers --field, the repeatable content-field
// predicate the memory read commands share. One grammar, parsed by one parser,
// so the help text has to be one string.
func MemoryFieldFilterFlag(cmd *cobra.Command, target *[]string) {
	cmd.Flags().StringSliceVar(target, "field", nil,
		"Content field filter: path=val, path>val, path>=val, path<val, path<=val, path? (nil), path! (not nil)")
}

// EntryLimitFlag registers --limit for the unranked reads that walk entries in
// storage order. The window is wide because the user is scanning: a filtered
// listing that stopped at a screenful would hide the entry being looked for
// behind a flag the user has no reason to suspect.
//
// Every command registering it must pair it with TruncationNotice: the window
// being wide is not the same as the window being enough, and a listing that
// stopped at it has to say so.
func EntryLimitFlag(cmd *cobra.Command, target *int) {
	cmd.Flags().IntVar(target, "limit", 100, "Max entries to return")
}

// ResultLimitFlag registers --limit for the ranked reads that return scored
// results. The window is narrower than EntryLimitFlag's because relevance
// decays down the list — past a screenful the rows are noise, not results.
//
// Decaying relevance is a reason for a SMALL window, not for a silent one: a
// reader still cannot tell "20 matched" from "20 shown of 500" without being
// told. Pair it with TruncationNotice too.
func ResultLimitFlag(cmd *cobra.Command, target *int) {
	cmd.Flags().IntVar(target, "limit", 20, "Max results to return")
}

// TruncationNotice writes the line a listing owes its reader when --limit, and
// not the data, is what ended it: which limit stopped it, and a concrete larger
// one that will not. It lives beside the two flags that create the ambiguity so
// a command cannot register one without meeting the other.
//
// noun names what was counted ("entries", "results"), so the sentence reads on
// both the unranked and the ranked listings.
//
// Call it ONLY when the read actually reported truncation
// (memory.QueryResult.Truncated / memory.MergedSearchResult.Truncated). A
// notice under every complete listing is noise, and a reader who learns to skip
// it has lost the signal for the listing that really was cut.
func TruncationNotice(w io.Writer, noun string, limit int) error {
	remedy := "a larger --limit"
	// Doubling is the suggestion; a limit past half of MaxInt cannot be doubled
	// and gets the wordy form rather than a negative number cobra would refuse.
	if limit > 0 && limit <= math.MaxInt/2 {
		remedy = fmt.Sprintf("--limit %d", limit*2)
	}
	_, err := fmt.Fprintf(w,
		"NOTE: stopped at the --limit of %d; more %s matched and are not shown.\n"+
			"      Re-run with %s to see them.\n\n",
		limit, noun, remedy)
	return err
}

// RenderFormatFlag registers --format, the renderer the toolspec-printing
// commands select. usagePrefix is for a command that honours the flag on only
// one of its paths; it is empty where --format always applies.
func RenderFormatFlag(cmd *cobra.Command, target *string, usagePrefix string) {
	cmd.Flags().StringVar(target, "format", "text", usagePrefix+"render format: text|markdown|json")
}

// DryRunClient renders the plan and touches nothing. It is the only mode the
// cluster-mutating commands accept; the flag stays a named mode rather than a
// bool so a server-side mode can be added without changing its type.
const DryRunClient = "client"

// DryRunModeFlag registers --dry-run for the commands that can print their
// plan instead of carrying it out. Registered here because `oap install` and
// `oap clean` must accept exactly the same modes: one of them taking a value
// the other refuses is a papercut the user finds only by being refused.
func DryRunModeFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "dry-run", "",
		DryRunClient+" (print the plan without touching the cluster)")
}

// ValidateDryRunMode rejects any mode but DryRunClient. The empty default means
// "not a dry run" and is not an error.
//
// Call it before the command does any work: an unrecognized mode discovered
// late means the user has already waited through cluster reads, or answered a
// destructive confirmation prompt, for a run that was never going to proceed.
func ValidateDryRunMode(mode string) error {
	if mode == "" || mode == DryRunClient {
		return nil
	}
	return fmt.Errorf("unknown --dry-run mode %q (want %q)", mode, DryRunClient)
}
