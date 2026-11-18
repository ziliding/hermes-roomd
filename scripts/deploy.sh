#!/usr/bin/env bash
# Build roomd and install it on the homelab.
#
#   mise run deploy                  # test, build, upload, restart
#   mise run deploy -- --no-restart  # upload only
#   ROOMD_HOST=otherbox mise run deploy
#
# What it copies. deploy/ in this repo is the source of truth; edit there,
# not on the homelab:
#   roomd binary         -> ~/.local/bin/roomd         (atomic rename; safe while running)
#   deploy/roomd.yaml    -> ~/.config/roomd/roomd.yaml (git-ignored; your real config)
#   deploy/roomd.env     -> ~/.config/roomd/roomd.env  (git-ignored; secrets, chmod 600)
#   deploy/rules/        -> ~/.config/roomd/rules/     (mirrored: files removed here are removed there)
#   deploy/roomd.service -> ~/.config/systemd/user/roomd.service
set -euo pipefail

host="${ROOMD_HOST:-homelab}"
restart=1
for arg in "$@"; do
  case "$arg" in
    --no-restart) restart=0 ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

cd "$(dirname "$0")/.."
step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

step "Checking $host"
arch=$(ssh "$host" uname -m)
case "$arch" in
  x86_64) goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  *) echo "unsupported architecture on $host: $arch" >&2; exit 1 ;;
esac

for f in deploy/roomd.yaml deploy/roomd.env deploy/rules/rules.md; do
  if [[ ! -f $f ]]; then
    echo "missing $f: create it first (from deploy/roomd.example.yaml, deploy/roomd.env.example)" >&2
    exit 1
  fi
done

version=$(git describe --always --dirty)
bin="dist/roomd-linux-$goarch"
step "Building $version for linux/$goarch"
mkdir -p dist
GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$bin" ./cmd/roomd


step "Checking deploy/roomd.yaml"
# Load it exactly as roomd will (rules files, env vars), with deploy/roomd.env's
# variables in a subshell, so a broken config never reaches $host.
(set -a; . deploy/roomd.env; set +a; go run ./cmd/roomd -check -config deploy/roomd.yaml)

# Show what changes in the config before replacing it (the env's values stay hidden).
if ! ssh "$host" 'cat ~/.config/roomd/roomd.yaml 2>/dev/null' | diff -u --label "$host roomd.yaml" --label "deploy/roomd.yaml" - deploy/roomd.yaml; then
  echo "(roomd.yaml on $host is replaced by deploy/roomd.yaml)"
fi
if ! diff <(ssh "$host" "sed -n 's/=.*//p' ~/.config/roomd/roomd.env 2>/dev/null") <(sed -n 's/=.*//p' deploy/roomd.env) >/dev/null; then
  echo "(roomd.env on $host has different variable names; it is replaced by deploy/roomd.env)"
fi

step "Uploading to $host"
ssh "$host" 'mkdir -p ~/.local/bin ~/.config/roomd ~/.config/systemd/user'
scp -q "$bin" "$host:.local/bin/roomd.new"
scp -q deploy/roomd.yaml "$host:.config/roomd/roomd.yaml"
ssh "$host" 'umask 077 && cat > ~/.config/roomd/roomd.env.new && mv ~/.config/roomd/roomd.env.new ~/.config/roomd/roomd.env' < deploy/roomd.env
# Mirror deploy/rules/ (no macOS ._ files or extended attributes in the tarball).
COPYFILE_DISABLE=1 tar --no-xattrs --no-mac-metadata -C deploy -cf - rules | ssh "$host" \
  'cd ~/.config/roomd && rm -rf rules.new && mkdir rules.new && tar -xf - -C rules.new --strip-components=1 && rm -rf rules && mv rules.new rules'
scp -q deploy/roomd.service "$host:.config/systemd/user/roomd.service"
ssh "$host" 'chmod +x ~/.local/bin/roomd.new && mv ~/.local/bin/roomd.new ~/.local/bin/roomd && systemctl --user daemon-reload'

if (( ! restart )); then
  step "Installed $version (not restarted)"
  exit 0
fi

step "Restarting roomd"
if ssh "$host" 'systemctl --user is-enabled --quiet roomd'; then
  ssh "$host" 'systemctl --user restart roomd'
else
  echo "roomd is not enabled yet; enabling it now"
  ssh "$host" 'systemctl --user enable --now roomd'
fi
sleep 3
if ssh "$host" 'systemctl --user is-active --quiet roomd'; then
  step "roomd $version is running on $host"
  ssh "$host" 'journalctl --user -u roomd -n 5 --no-pager -o cat'
else
  step "roomd failed to start on $host"
  ssh "$host" 'journalctl --user -u roomd -n 30 --no-pager -o cat'
  exit 1
fi
