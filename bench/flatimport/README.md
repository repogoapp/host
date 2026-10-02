# FlatBuffers and host thread import

This isolated Go module measures whether a normalized FlatBuffers event cache
helps the host import provider transcripts into SQLite. It does not change the
host, its dependencies, the provider files, or the RPC protocol.

Run from this directory:

```sh
go test -run TestRoundTrip -count=1
go test -run '^$' -bench '^BenchmarkImport$' -benchmem -benchtime=5x -count=3
```

Each provider gets 64 synthetic threads with 40 turns each: prompts, assistant
prose, tool calls, and tool outputs, including Unicode. The real Claude/Codex
providers discover and parse temporary JSONL files. Each measured import writes
10,240 normalized events through the production `store.SyncBatch`, using a fresh
temporary SQLite database and the normal batch size of 64.

| Case | Measured work |
| --- | --- |
| `jsonl_initial` | Read and parse source JSONL, write SQLite |
| `jsonl_initial_build_flatcache` | Read and parse source JSONL, construct/write a new FlatBuffers cache, write SQLite |
| `flatcache_rebuild` | Read an existing FlatBuffers cache, materialize Go events, write SQLite |
| `normalized_jsoncache_rebuild` | Read an existing normalized JSON cache using Sonic, materialize Go events, write SQLite |
| `sqlite_write_only` | Write already materialized Go events to SQLite |

Cache generation for the rebuild cases is excluded intentionally. The first
import with cache construction measures that cost explicitly. Rebuilds require
an existing valid cache; no validation/invalidation machinery is implemented.
An ordinary host restart already skips unchanged sessions using SQLite
fingerprints, so cache rebuild results do not describe the normal restart path.

Discovery, schema/database creation, verification, and cleanup are outside the
timed region. These are import-stage measurements, not full application startup
or host-to-phone initial sync. Files are warm in the OS cache. Cache writes use
`os.WriteFile` without fsync. SQLite uses production settings. The fixture does
not reproduce large histories, resumed sessions, subagents, attachments, or the
full distribution of real tool output sizes. No real history or credentials are
read. The benchmark uses the Go parser, not the optional native Rust parser.

FlatBuffers uses generated object APIs plus explicit conversion to the host's
existing event structs; conversions, allocations, and buffer construction are
included. This is a straightforward prototype, not the minimum possible
allocation implementation. Tool input stays opaque JSON bytes in both formats.
Every parsed fixture is checked for exact event equality after binary round trip,
and each import checks stored thread/event counts. `TestRoundTrip` also covers
usage, approvals/questions, errors, and other fields absent from the fixture.
The generated reader consumes trusted, self-generated data; this is not an
untrusted-wire validation implementation.

The schema is `events.fbs`. Bindings were generated with FlatBuffers release
`v25.12.19-2026-02-06-03fffb2` (`flatc --go --gen-object-api -o . events.fbs`),
using the Go runtime pinned in `go.mod`. Regenerate the mappings with
`python3 generate_mapping.py`, then `gofmt -w mapping.go`.

See `results-2026-09-29.txt` for raw repeated measurements and
`results-2026-09-29.md` for interpretation. The module is intentionally separate
from the host: its checks must be run explicitly and are not included by the
host's `go test ./...`.
