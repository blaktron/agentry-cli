Conformance fixtures (plan runner-m-r4-adapters §7), one directory per
adapter: `<case>.json` goldens of what `Compile` produces for the shared
Input (`go test ./pkg/runner/adapter -update` rewrites them; check the
diff first), `events.ndjson` a recording of the harness's stdout, and
`expect.json` the event shape the engine relies on. The docker-agent
recording is the full `receipts/events.ndjson` of one hello-report run
under docker-agent 1.128.0 on the dev host (2026-09-07, the allowed run of
`scripts/acceptance/runner-r4.sh`); re-record the same way when the pinned
harness changes.
