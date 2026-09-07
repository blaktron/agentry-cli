Conformance fixtures (plan runner-m-r4-adapters §7), one directory per
adapter: `<case>.json` goldens of what `Compile` produces for the shared
Input (`go test ./pkg/runner/adapter -update` rewrites them; check the
diff first), `events.ndjson` a recording of the harness's stdout, and
`expect.json` the event shape the engine relies on.

Each recording is the full `receipts/events.ndjson` of one real hello-report
run on the dev host (2026-09-07), taken from the allowed run of
`scripts/acceptance/runner-r4.sh`: docker-agent 1.128.0 for `docker-agent`,
Claude Code 2.1.263 for `claude`. Re-record the same way when a harness
release changes — `AGENTRY_R4_EVIDENCE=<dir>` on that script keeps each
adapter's receipts for exactly this purpose. A recording is what keeps the
event mapping honest: these schemas are nobody's stable contract, so the
suite pins them to output a harness actually produced rather than to what
its documentation says it produces.
