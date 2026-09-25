# tui

`oap`'s terminal design system: a thin layer over `charmbracelet/huh`. huh owns
the form *fields*; this package owns **sequencing, chrome, theme and driver
selection**. The rationale for each of those choices is in
[`doc.go`](doc.go) — this file is the map of what is in the directory.

A wizard is a `[]Screen`, not one big `huh.Form`, so branching and between-question
work live in ordinary testable Go. The same `[]Screen` renders three ways.

| File | Holds |
| --- | --- |
| [`screen.go`](screen.go) | The `Screen` interface (`ID`/`Label`/`Prepare`/`Apply`) and `ErrSkip`. |
| [`run.go`](run.go) | `Options` and `Run` — the sequencer that walks screens, presents, and applies. `Options.Inline` carries the rule for which TTY shape a run gets. |
| [`state.go`](state.go) | `State`, the answer bag carried across screens, and the `Note` entries the summary is built from. |
| [`driver.go`](driver.go) | The `Driver` interface and `DriverFor` — the one place the three-way renderer choice is made. |
| [`driver_tty.go`](driver_tty.go) | bubbletea with chrome; full-screen or inline. |
| [`driver_plain.go`](driver_plain.go) | huh's accessible mode over an `io.Reader`/`io.Writer`. Used off-TTY, under `NO_COLOR`, and by every test — which is why wizard tests need no pseudo-terminal. |
| [`driver_noninteractive.go`](driver_noninteractive.go) | Refuses to prompt; returns `ErrUnanswered`. |
| [`caps.go`](caps.go) | `Caps` and `Detect` — the one place TTY-ness, color and width are established. |
| [`theme.go`](theme.go) | The whole color palette and the `Theme` that yields both a `*huh.Theme` and lipgloss styles, so the two cannot drift. |
| [`chrome.go`](chrome.go) | The step rail: `Step`, `Steps` (derived from the screens, never transcribed), and the width floors below which the rail is dropped. |
| [`question.go`](question.go) | `Question` — the guidance-block-plus-one-answer screen nearly every wizard step turned out to be. A screen with two interactive fields is deliberately not expressible. |
| [`note.go`](note.go) | `NoteBudget` — how many display columns a huh note's text actually gets, measured at an 80-column floor. |
| [`table.go`](table.go), [`summary.go`](summary.go) | Themed columnar output for list/detail commands, and the post-run summary block that stays in scrollback. |
| [`userfacing.go`](userfacing.go) | Strips this package's own `tui: …` framing off an error before a person sees it. |

## Constraints

- **Every `Screen.Prepare` must consult `State`** and return a nil group when its
  key is already answered. That one convention is what makes non-interactive
  mode work: seed `State` from flags, and any screen that still wants to ask has
  proved an input was missing.
- **`Prepare` must build a new `*huh.Group` on every call.** A group is stateful
  and presenting it mutates it in place; never cache or reuse one.
- **No caller branches on `Caps.TTY`.** `Detect` establishes it, `DriverFor` acts
  on it. Two commands cannot disagree about which renderer a terminal gets.
- **Ctrl+C never reads as an answer** under either interactive driver, but it
  arrives by different routes — SIGINT under plain (cooked mode), a form-state
  flag under TTY (raw mode), which is why the TTY driver reads that state and
  returns `huh.ErrUserAborted`.

Part of [`pkg/cli`](../README.md). See the root [`README.md`](../../../README.md)
and [`AGENTS.md`](../../../AGENTS.md).
