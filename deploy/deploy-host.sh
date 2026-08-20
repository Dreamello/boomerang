#!/usr/bin/env bash
# Install the boomerang agent on one relay/destination host.
#
# Hash-gated: a truncated scp installs silently and segfaults later, so nothing
# is installed unless the remote hash matches the local one.
#
# Usage: deploy-host.sh <ssh-target> <binary> <key> [options...]
#   --proxy            route SSH/scp through a local SOCKS5 on 127.0.0.1:11111,
#                      for hosts requiring a proxy connection.
#   --jump <spec>      SSH jump host(s), passed to -J. Comma-separate for a
#                      multi-hop chain (e.g. relay.example,jump.example).
#   --identity <path>  private key for the target, passed to -i.
#
# Use --jump for hosts that require a jump host; the same install and key
# permissions apply to direct and jumped connections.
set -euo pipefail

TARGET="$1"; BIN="$2"; KEY="$3"; shift 3
SSH_OPTS=(-o ConnectTimeout=25 -o BatchMode=yes)

while [ $# -gt 0 ]; do
  case "$1" in
    --proxy)
      SSH_OPTS+=(-o "ProxyCommand=nc -X 5 -x 127.0.0.1:11111 %h %p"); shift ;;
    --jump)
      [ $# -ge 2 ] || { echo "--jump needs a value" >&2; exit 2; }
      SSH_OPTS+=(-J "$2"); shift 2 ;;
    --identity)
      [ $# -ge 2 ] || { echo "--identity needs a value" >&2; exit 2; }
      SSH_OPTS+=(-i "$2"); shift 2 ;;
    *)
      echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

WANT_BIN=$(shasum -a 256 "$BIN" | awk '{print $1}')
WANT_KEY=$(shasum -a 256 "$KEY" | awk '{print $1}')
echo "== $TARGET"
echo "   want bin=${WANT_BIN:0:12} key=${WANT_KEY:0:12}"

push() { # local remote want
  local src="$1" dst="$2" want="$3" got
  for attempt in 1 2 3; do
    scp "${SSH_OPTS[@]}" -q "$src" "$TARGET:$dst" || true
    got=$(ssh "${SSH_OPTS[@]}" "$TARGET" "sha256sum '$dst' 2>/dev/null | awk '{print \$1}'" || true)
    if [ "$got" = "$want" ]; then
      echo "   ok   $(basename "$dst") hash matches (attempt $attempt)"
      return 0
    fi
    echo "   retry $(basename "$dst") hash mismatch (got ${got:-none})"
    ssh "${SSH_OPTS[@]}" "$TARGET" "rm -f '$dst'" || true
  done
  echo "   FATAL could not transfer $(basename "$dst") intact" >&2
  return 1
}

push "$BIN" /tmp/boomerang.new "$WANT_BIN"
push "$KEY" /tmp/boomerang.key.new "$WANT_KEY"
push deploy/boomerang-agent.service /tmp/boomerang-agent.service.new \
  "$(shasum -a 256 deploy/boomerang-agent.service | awk '{print $1}')"

# Install and start. sudo -n so a missing NOPASSWD fails loudly, never hangs.
SUDO=""
if [ "$(ssh "${SSH_OPTS[@]}" "$TARGET" 'id -u')" != "0" ]; then SUDO="sudo -n"; fi

ssh "${SSH_OPTS[@]}" "$TARGET" "set -e
  $SUDO install -m755 -o root -g root /tmp/boomerang.new /usr/local/bin/boomerang
  # 0640 root:<login group> is one key serving both readers: systemd reads it as
  # root for LoadCredential=, and the logged-in user can run the CLI unflagged.
  # Root could read any copy anyway, so a second file under \$HOME would widen
  # exposure without adding access.
  $SUDO install -m640 -o root -g \"\$(id -gn)\" /tmp/boomerang.key.new /etc/boomerang.key
  $SUDO install -m644 -o root -g root /tmp/boomerang-agent.service.new \
       /etc/systemd/system/boomerang-agent.service
  rm -f /tmp/boomerang.new /tmp/boomerang.key.new /tmp/boomerang-agent.service.new
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable --now boomerang-agent.service
  # enable --now is a no-op on an already-running unit, so restart to be sure
  # this binary and key are the ones loaded.
  $SUDO systemctl restart boomerang-agent.service
"
sleep 1.5
echo "   --- post-install state ---"
ssh "${SSH_OPTS[@]}" "$TARGET" "
  systemctl show boomerang-agent.service -p ActiveState -p SubState -p MainPID | tr '\n' ' '; echo
  sha256sum /usr/local/bin/boomerang | awk '{print \"   installed bin=\" substr(\$1,1,12)}'
  sudo -n sha256sum /etc/boomerang.key 2>/dev/null | awk '{print \"   installed key=\" substr(\$1,1,12)}' || true
  ss -ulnp 2>/dev/null | grep 8888 || echo '   NOT LISTENING'
"
