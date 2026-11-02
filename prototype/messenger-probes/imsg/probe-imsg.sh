#!/bin/bash
# PROTOTYPE, throwaway: what imsg needs and does when a persistent agent uses
# iMessage on a Mac. Run it in Terminal, in the macOS account whose Messages is
# signed in. It prints no message text, only lengths, times and flags.
#
#   IMSG=/path/to/imsg ./probe-imsg.sh <handle the phone sends from> [watch minutes]
#   IMSG=/path/to/imsg ./probe-imsg.sh --launchd     # step 4 alone
set -u
IMSG=${IMSG:-imsg}
OUT=$(mktemp -d)
now() { date -u +%H:%M:%S; }

launchd_step() {
  echo "== 4. launched by launchd, not Terminal: does Terminal's grant still apply?"
  BIN=$(command -v "$IMSG")
  launchctl submit -l net.secureagentry.probe-imsg -o "$OUT/ld.out" -e "$OUT/ld.err" -- "$BIN" chats --limit 1 --json
  sleep 5
  launchctl remove net.secureagentry.probe-imsg 2>/dev/null
  if [ -s "$OUT/ld.out" ]; then
    echo "launchd read chat.db: OK"
  else
    echo "launchd read chat.db: DENIED: $(head -c 300 "$OUT/ld.err")"
  fi
}

if [ "${1:-}" = --launchd ]; then
  launchd_step
  exit
fi
HANDLE=${1:?the phone number or email the phone sends from}
MINUTES=${2:-5}

echo "== versions"
sw_vers | tr '\n' ' '; echo; uname -m
"$IMSG" --version

echo "== 1. Full Disk Access, through Terminal's grant"
if "$IMSG" chats --limit 1 --json >"$OUT/chats" 2>"$OUT/chats.err"; then
  echo "read chat.db: OK ($(wc -l <"$OUT/chats" | tr -d ' ') chat line)"
else
  echo "read chat.db: DENIED: $(head -c 300 "$OUT/chats.err")"
fi

echo "== 2. inbound: send messages from the phone to this Mac now ($MINUTES min)"
echo "   (one plain text, one photo, and one tapback if you can)"
perl -e 'alarm shift; exec @ARGV' $((MINUTES * 60)) \
  "$IMSG" watch --participants "$HANDLE" --reactions --attachments --json 2>"$OUT/watch.err" |
  while IFS= read -r line; do
    printf '%s %s\n' "$(now)" "$(printf '%s' "$line" | /usr/bin/jq -c '{id, chat_id, is_group, is_from_me, sender, type, is_reaction, created_at, text_len: (.text // "" | length), attachments: (.attachments | length), participants}')"
  done
echo "   watch stderr: $(head -c 300 "$OUT/watch.err")"

echo "== 3. outbound: Automation of Messages (approve the prompt if one appears)"
if "$IMSG" send --to "$HANDLE" --text "agentry probe $(now)" --json >"$OUT/send" 2>"$OUT/send.err"; then
  echo "send: $(cat "$OUT/send")"
else
  echo "send: FAILED: $(head -c 300 "$OUT/send.err")"
fi

launchd_step
echo "   Now add $BIN itself to Full Disk Access in System Settings and run"
echo "   ./probe-imsg.sh --launchd, to see whether a launchd-started binary"
echo "   carries its own grant."
echo "== done; raw files in $OUT"
