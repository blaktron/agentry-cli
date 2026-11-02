#!/bin/sh
# PROTOTYPE: the messenger probe's rig on the dev host. Containers and networks are named probe-msgr-*.
set -eu
L4=docker/mcp-l4proxy@sha256:83c560e8938f6218e486b732469f49bd753dc832f0488a7386be4540242fff82
B=$HOME/scratch/messenger-m0/longconn
RC=$HOME/scratch/messenger-m0/resolv.conf
grep '^nameserver' /run/systemd/resolve/resolv.conf > $RC   # docker's 127.0.0.11 does not answer under runsc
MIN=${1:-60}
docker image inspect $L4 >/dev/null 2>&1 || docker pull -q $L4
docker rm -f probe-msgr-up probe-msgr-relay-up probe-msgr-relay-discord probe-msgr-relay-tg probe-msgr-client >/dev/null 2>&1 || true
docker network rm probe-msgr-int probe-msgr-out >/dev/null 2>&1 || true
docker network create --internal probe-msgr-int >/dev/null
docker network create probe-msgr-out >/dev/null
docker run -d --name probe-msgr-up --network probe-msgr-out -v $B:/probe:ro --entrypoint /probe $L4 server :8080 >/dev/null
UPIP=$(docker inspect -f '{{(index .NetworkSettings.Networks "probe-msgr-out").IPAddress}}' probe-msgr-up)
relay() { # name host port [add-host]
  docker create --name $1 --runtime runsc --network probe-msgr-out -v $RC:/etc/resolv.conf:ro --cap-drop ALL ${4:+--add-host $4} \
    -e PROXY_HOSTNAME=$2 -e PROXY_PORTS=$3 $L4 >/dev/null
  docker network connect probe-msgr-int $1
  docker start $1 >/dev/null
  docker inspect -f '{{(index .NetworkSettings.Networks "probe-msgr-int").IPAddress}}' $1
}
R1=$(relay probe-msgr-relay-up probe-upstream 8080 probe-upstream:$UPIP)
R2=$(relay probe-msgr-relay-discord gateway.discord.gg 443)
R3=$(relay probe-msgr-relay-tg api.telegram.org 443)
sleep 8
docker run -d --name probe-msgr-client --runtime runsc --network probe-msgr-int --cap-drop ALL --user 10001 \
  --add-host probe-upstream:$R1 --add-host gateway.discord.gg:$R2 --add-host api.telegram.org:$R3 \
  -v $B:/probe:ro -v /etc/pki/tls/certs/ca-bundle.crt:/ca.crt:ro -e SSL_CERT_FILE=/ca.crt \
  --entrypoint /probe $L4 client $MIN >/dev/null
echo "upstream $UPIP relays $R1 $R2 $R3; client started for $MIN min at $(date -u +%H:%M:%S)"
