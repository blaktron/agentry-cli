# context-measure

Reads the receipts of runs and of persistent agent segments and reports how
large each conversation's context grew, which tools' results filled it, and
what clearing old tool results after a pause would have removed.

```bash
go run ./scripts/context-measure [flags] <receipts folder or events.ndjson>...
```

Every `events.ndjson` at or under each argument is read:

- **A run's events** are the harness's own lines. They are read with the
  adapter that `run-plan.json` beside them names, or `-adapter` when there is
  none, so a run on any harness is measured the same way.
- **A session's events** (an interactive run, a persistent agent's segment)
  are the session records. Context sizes come from their usage events. Tool
  results are read from docker-agent's ACP messages; another harness's session
  results are not counted, and the report says so.

For each file it prints the model requests and turns, the context at the
first request, at its peak and summed over every request, the tool calls and
the bytes each tool returned, the largest turn, and every **cold start**: a
turn that began after a pause longer than `-after`. For a cold start it shows
the context the turn's first request resent and the bytes of tool results a
clear would have replaced, keeping whole the results of the newest
`-keep-turns` turns and the newest `-keep-browser` browser results (tools named
with `-browser-prefix`). `-json` prints the same as one JSON array.

How the figures are read:

- **A pause** runs from the model's last activity (its text, a usage report,
  a tool call or result) to the person's next message. The runner's own
  records, a notice or a note carried into the turn, do not end it. A pause
  inside a turn, such as a long tool call, is never a cold start, and a run
  is one turn, so only sessions have cold starts.
- **The kept turns** are the newest turns before the cold one, since the
  cold turn has no results yet. The newest browser results are kept wherever
  they fall; older ones go even inside a kept turn.
- **A conversation that shrinks** by more than a tenth, or that the harness
  says it compacted, is counted as a restart, and the results before it no
  longer count as held.
- **A run's context** is each usage event's input as the harness reported
  it. That is per request on docker-agent but per turn or per run
  on others, and Claude reports input without its cache reads, so a run's
  figures on those harnesses are understated.
- **Tool names** are printed only when they are a harness's identifier;
  anything else is shown as `(unnamed)`. No result text, argument or title is
  printed.

| Flag | Default | |
|---|---|---|
| `-after` | `5m` | a pause longer than this before a turn is a cold start |
| `-keep-turns` | `2` | the newest turns whose results a clear keeps |
| `-keep-browser` | `2` | the newest browser results a clear keeps |
| `-browser-prefix` | `browser_` | the browser server's tool name prefix |
| `-adapter` | | the adapter for a run's events with no `run-plan.json` |
| `-top` | `8` | tools listed per file |
| `-json` | off | JSON instead of the table |

Test it with `go test ./scripts/context-measure/`.
