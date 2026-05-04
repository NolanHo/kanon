# macOS foreground client guide

This guide shows how to run the `kanon` client in a macOS terminal foreground session so it can be started and stopped manually.

Use stream mode. Stop with `Ctrl+C`. Start again with the same command. The client resumes from the saved cursor under `~/Library/Application Support/kanon/`.

## When the server port is not directly reachable

If the Linux host only accepts SSH and does not expose the HTTP control port directly, do not create a manual tunnel first.

Use the built-in tunnel flags instead:

- `-server http://127.0.0.1`
- `-tunnel-host <ssh-host>`
- `-tunnel-remote-port 39090`

The client will:

- start an SSH local port forward itself
- auto-pick a free local port above `30000` by default
- connect the HTTP control plane through that local forwarded port
- keep the file data plane on `rsync` or HTTP as configured

## Preconditions

The macOS machine must have:

- Go installed
- SSH access to the server host used by `rsync` and the tunnel
- `rsync` available locally

Quick checks:

```bash
ssh server-host 'echo ok'
/opt/homebrew/bin/rsync --version
```

## Clone or update the repo

```bash
mkdir -p "$HOME/code"
cd "$HOME/code"
if [ -d kanon/.git ]; then
  cd kanon && git pull --ff-only
else
  git clone https://github.com/NolanHo/kanon.git
  cd kanon
fi
```

## Build the client binary

```bash
cd "$HOME/code/kanon"
go build -o bin/kanon-client ./cmd/kanon-client
```

## One-shot validation

Run this once before the long-lived stream. It validates SSH, tunnel setup, local path permissions, and transfer mode.

```bash
cd "$HOME/code/kanon"
./bin/kanon-client \
  -server http://127.0.0.1 \
  -tunnel-host server-host \
  -tunnel-remote-port 39090 \
  -local-root "$HOME/Documents/kanon" \
  -state-dir "$HOME/Library/Application Support/kanon" \
  -sync-mode auto \
  -rsync-source server-host:/root/docs/ \
  -rsync-bin /opt/homebrew/bin/rsync \
  -json
```

Expected result:

- if `rsync` works, output JSON includes `"transfer_mode":"rsync"`
- if `rsync` is unavailable or fails, output JSON includes `"transfer_mode":"http"` and a `fallback_reason`

## Foreground stream command

This is the normal persistent foreground mode:

```bash
cd "$HOME/code/kanon"
./bin/kanon-client \
  -stream \
  -server http://127.0.0.1 \
  -tunnel-host server-host \
  -tunnel-remote-port 39090 \
  -local-root "$HOME/Documents/kanon" \
  -state-dir "$HOME/Library/Application Support/kanon" \
  -sync-mode auto \
  -rsync-source server-host:/root/docs/ \
  -rsync-bin /opt/homebrew/bin/rsync \
  -stream-poll-interval 1s \
  -debounce 1s \
  -reconnect 5s
```

If you want a fixed local forwarded port instead of auto-selection, add:

```bash
-tunnel-local-port 30081
```

## Foreground output

Startup now prints a banner instead of one long line, for example:

```text
kanon  dev
  control: http://127.0.0.1:30081
  tunnel:  ssh server-host -> 127.0.0.1:39090
  local:   /Users/your-user/Documents/kanon
  state:   /Users/your-user/Library/Application Support/kanon
  data:    auto rsync=server-host:/root/docs/ fallback=http
```

When a batch arrives, output is grouped:

```text
01:02:15 sync rsync put=2 del=1
  PUT README.md
  PUT docs/guide.md
  DEL old.md
```

If `auto` had to fall back to HTTP for that batch, the summary line includes `fallback`.

## Alias to add

Add this line to `~/.zshrc` or `~/.bashrc`:

```bash
alias kanon-docs='cd "$HOME/code/kanon" && SERVER=http://127.0.0.1 TUNNEL_HOST=server-host TUNNEL_REMOTE_PORT=39090 LOCAL_ROOT="$HOME/Documents/kanon" STATE_DIR="$HOME/Library/Application Support/kanon" SYNC_MODE=auto RSYNC_SOURCE=server-host:/root/docs/ RSYNC_BIN=/opt/homebrew/bin/rsync STREAM=1 ./scripts/run-kanon-client.sh'
```

Reload shell config:

```bash
source ~/.zshrc
```

Then run:

```bash
kanon-docs
```

## State and logs

Client state directory:

- `~/Library/Application Support/kanon/cursor`
- `~/Library/Application Support/kanon/client.lock`
- `~/Library/Application Support/kanon/client.log`

Inspect logs:

```bash
tail -f "$HOME/Library/Application Support/kanon/client.log"
```

## Failure diagnosis

If startup fails:

1. verify SSH:

```bash
ssh server-host 'echo ok'
```

2. verify `rsync`:

```bash
/opt/homebrew/bin/rsync --version
```

3. if tunnel startup fails, force a known local port and test manually:

```bash
cd "$HOME/code/kanon"
./bin/kanon-client \
  -stream \
  -server http://127.0.0.1 \
  -tunnel-host server-host \
  -tunnel-remote-port 39090 \
  -tunnel-local-port 30081 \
  -local-root "$HOME/Documents/kanon" \
  -state-dir "$HOME/Library/Application Support/kanon" \
  -sync-mode auto \
  -rsync-source server-host:/root/docs/ \
  -rsync-bin /opt/homebrew/bin/rsync
```

4. if `rsync` keeps failing, force HTTP mode to isolate transfer issues:

```bash
cd "$HOME/code/kanon"
./bin/kanon-client \
  -stream \
  -server http://127.0.0.1 \
  -tunnel-host server-host \
  -tunnel-remote-port 39090 \
  -local-root "$HOME/Documents/kanon" \
  -state-dir "$HOME/Library/Application Support/kanon" \
  -sync-mode http
```

If HTTP mode works and `auto` does not, the fault is in the `rsync` path, not in the event stream.
