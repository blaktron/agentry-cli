#!/bin/sh
# PROTOTYPE: whatsmeow behind one relay to web.whatsapp.com:443 under runsc; store on the volume probe-msgr-wa.
set -eu
L4=docker/mcp-l4proxy@sha256:83c560e8938f6218e486b732469f49bd753dc832f0488a7386be4540242fff82
B=$HOME/scratch/messenger-m0/wa
RC=$HOME/scratch/messenger-m0/resolv.conf
docker rm -f probe-msgr-relay-wa probe-msgr-wa >/dev/null 2>&1 || true
docker volume create probe-msgr-wa >/dev/null
docker run --rm --user 0 -v probe-msgr-wa:/data --entrypoint chown $L4 10001 /data
docker create --name probe-msgr-relay-wa --runtime runsc --network probe-msgr-out -v $RC:/etc/resolv.conf:ro --cap-drop ALL \
  -e PROXY_HOSTNAME=web.whatsapp.com -e PROXY_PORTS=443 $L4 >/dev/null
docker network connect probe-msgr-int probe-msgr-relay-wa
docker start probe-msgr-relay-wa >/dev/null
R=$(docker inspect -f '{{(index .NetworkSettings.Networks "probe-msgr-int").IPAddress}}' probe-msgr-relay-wa)
sleep 8
docker run -d --name probe-msgr-wa --runtime runsc --network probe-msgr-int --cap-drop ALL --user 10001 \
  --add-host web.whatsapp.com:$R -v probe-msgr-wa:/data -v $B:/probe:ro \
  -v /etc/pki/tls/certs/ca-bundle.crt:/ca.crt:ro -e SSL_CERT_FILE=/ca.crt \
  --entrypoint /probe $L4 /data/wa.db "$@" >/dev/null
echo "wa started behind relay $R"
