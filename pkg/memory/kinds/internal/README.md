# `kinds/internal`

Boilerplate shared across the memory Kinds. Not importable outside
`pkg/memory/kinds`.

| Package | What it holds |
| ------- | ------------- |
| [`auditaccessor/`](auditaccessor/) | The append-one / list-all accessor body the simple per-scope audit Kinds share |
| [`undecodable/`](undecodable/) | The operator-facing report every Kind accessor owes when a stored Entry will not decode |

## `auditaccessor`

Each caller supplies the four things that vary: its `Content` type, its
`memory.Kind`, a label for error/log strings, and which of its own timestamp
fields carries the entry time. Nothing about serialization, Kind name, scope or
retention lives here — it all flows in from the calling Kind.

## `undecodable` — and why the disposition is a judgement call

`Memory.Put` validates an entry's Kind registration, the writer's authority and
the ID/link prefixes — **nothing about its shape**. `Kind.ContentSchema()` is
read at exactly one site (query field-path validation), never as a write-time
contract, and several Kinds declare none. So an authorized writer can store a
row no reader can parse — and for an append-only Kind nothing removes it
afterwards: per-entry `Delete` is refused and `DeleteScope` skips it.

Pick the disposition by what the row **means**:

| Disposition | When | Why |
| ----------- | ---- | --- |
| **Skipped** | The row is part of a *record* — a transcript, an audit list, a rendering stream | Erroring instead wedges every read of the scope forever, with no operator remedy |
| **Refused** | The row is an *input to a gate* — a taint, a denial, a recorded decision | Dropping it makes the gate forget what it recorded, re-opening exactly what it was written to close. Fail closed |

Both report the entry **and** its publisher: the ID says which record is missing
from a tamper-evident log, the publisher says who to go fix — the only remedy
once the row cannot be deleted. Silence is not an option for either.
