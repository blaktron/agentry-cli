#!/bin/sh
# PROTOTYPE: signal-cli (native build) linking as a device under runsc, behind relays to Signal's hosts only.
# Usage: run-signal.sh [hosts...]   (default chat.signal.org)
set -eu
L4=docker/mcp-l4proxy@sha256:83c560e8938f6218e486b732469f49bd753dc832f0488a7386be4540242fff82
IMG=debian:trixie-slim
D=$HOME/scratch/messenger-m0/signal
RC=$HOME/scratch/messenger-m0/resolv.conf
HOSTS=${*:-chat.signal.org}
docker rm -f probe-msgr-signal >/dev/null 2>&1 || true
docker volume create probe-msgr-signal >/dev/null
docker run --rm --user 0 -v probe-msgr-signal:/data --entrypoint chown $L4 10001 /data
ADD=""
for h in $HOSTS; do
  n=probe-msgr-relay-sig-$(echo $h | tr . -)
  docker rm -f $n >/dev/null 2>&1 || true
  docker create --name $n --runtime runsc --network probe-msgr-out -v $RC:/etc/resolv.conf:ro --cap-drop ALL \
    -e PROXY_HOSTNAME=$h -e PROXY_PORTS=443 $L4 >/dev/null
  docker network connect probe-msgr-int $n
  docker start $n >/dev/null
  ADD="$ADD --add-host $h:$(docker inspect -f '{{(index .NetworkSettings.Networks "probe-msgr-int").IPAddress}}' $n)"
done
sleep 8
docker run -d --name probe-msgr-signal --runtime runsc --network probe-msgr-int --cap-drop ALL --user 10001 $ADD \
  -e HOME=/data -v probe-msgr-signal:/data -v $D/signal-cli:/signal-cli:ro \
  --entrypoint /signal-cli $IMG --config /data/signal link -n agentry-probe >/dev/null
echo "signal-cli started behind relays for: $HOSTS"
