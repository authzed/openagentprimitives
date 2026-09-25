# `pkg/platform/extract`

Turns an uploaded file into agent-readable text. A MIME type selects a
registered `Extractor`, so a new format is a registration rather than a branch
in a caller. The root holds the interface, the `Result` shape, and the
MIME registry; each backend is a subpackage that self-registers.

This is the inbound mirror of `pkg/channels/channelassets` (which renders
outbound).

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`text`](text/) | Formats that are already text: `text/plain`, `text/markdown`, `text/csv`, `application/json`. Reads to EOF and coerces to valid UTF-8. |
| [`tabula`](tabula/) | PDF/DOCX/PPTX/HTML via `github.com/tsawler/tabula`. Writes input to a temp file for every format because tabula dispatches on filename. |
| [`extractordclient`](extractordclient/) | Not an extractor — the HTTP client `internal/cmd/operator` dials `internal/cmd/extractord` with. Implements `memory/httpsrv.AttachmentExtractor`. |

## Containers: `Exploder`

A container format (`.zip`) is not an `Extractor`. `Result{Text, Pages}`
cannot express N members, so containers implement `Exploder` and live in a
SEPARATE MIME registry — a MIME has an exploder or an extractor, never both,
and a caller must not get one where it asked for the other.

`Explode` takes `io.ReaderAt` + size (a container needs random access to read
its directory), yields members one at a time, and enforces `Limits` WHILE
STREAMING. Nesting depth is 0: a member that is itself an archive is yielded
like any other member and never opened.

Skips are reported as counts per `SkipReason`, never as names — a member name
is file content, and this pod has no business emitting file content.

## Constraints

- **Extractors run against untrusted uploads in a zero-egress pod.** No network
  I/O, ever — `extractord` has no egress and no credentials by design. A backend
  whose library needs a path may use `os.TempDir()` and must remove what it
  writes.
- **Malformed input must produce an error, never a panic.**
- **`Register` panics on a duplicate MIME claim.** Two backends silently
  competing would make which one runs depend on import order. The conflict
  surfaces at process startup instead.
- **The registered MIME set is deliberately narrower than what the libraries
  support.** XLSX, ODT and EPUB are unregistered: an unregistered MIME produces
  an honest "cannot be read" notice; a registered one that hangs or OOMs the
  service produces an outage. See `tabula`'s package doc for the concrete XLSX
  hazard.
- Each backend enforces its own input-size cap and returns `extract.ErrTooLarge`.
