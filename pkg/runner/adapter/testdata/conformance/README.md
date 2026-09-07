Conformance fixtures (plan runner-m-r4-adapters §7), one directory per
adapter: `<case>.json` goldens of what `Compile` produces for the shared
Input (`go test ./pkg/runner/adapter -update` rewrites them; check the
diff first), `events.ndjson` a recording of the harness's stdout, and
`expect.json` the event shape the engine relies on. The docker-agent
recording is assembled from lines observed from docker-agent 1.128.0
(agentry-cli#51, #53); `scripts/acceptance/runner-r4.sh` leaves a full
one in its receipts to replace it with.
