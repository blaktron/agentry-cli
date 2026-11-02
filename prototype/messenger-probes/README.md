# PROTOTYPE: messenger probes (throwaway)

Answers four questions before the messenger role is built. Not production code,
never merged into `dev`.

- `longconn/`: does a long-lived WebSocket and an HTTP long-poll hold through the
  gateway's TCP relay (`docker/mcp-l4proxy`, HAProxy) when the relay and the
  client run under gVisor? Also opens Discord's real gateway and Telegram's Bot
  API through their own relays. Build `CGO_ENABLED=0 go build`, copy the binary
  to a Docker host with `runsc`, run `run-longconn.sh <minutes>`, then
  `docker logs probe-msgr-client`.
- `wa/`: whatsmeow linking as a WhatsApp device from a gVisor container whose only
  route out is a relay to `web.whatsapp.com:443`, its store on a volume.
  `run-wa.sh` (QR) or `run-wa.sh +<number>` (phone code); run it again to see
  whether the session resumes.
- `signal/`: signal-cli's native build linking as a Signal device the same way.
  `run-signal.sh [hosts…]`.
- `imsg/`: `imsg` on a Mac, run in Terminal by the person whose Messages is
  signed in: Full Disk Access through Terminal's grant, inbound latency and
  shape from their phone, a send through Messages (Automation), and whether a
  launchd-started `imsg` reads with Terminal's grant or needs its own.
  `IMSG=/path/to/imsg ./probe-imsg.sh <handle> [minutes]`. It prints no
  message text.

Every container and network the scripts make is named `probe-msgr-*`; remove
them with `docker rm -f $(docker ps -aq --filter name=probe-msgr)` and
`docker network rm probe-msgr-int probe-msgr-out`.
