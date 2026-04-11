# macOS foreground client guide

This guide shows how to run the `vault-bridge` client in a macOS terminal foreground session so it can be started and stopped manually.

Replace the example values in this document with your own:

- server URL: `http://server-host:9090`
- `rsync` source root: `server-host:/srv/vault-bridge/source/`
- local vault path: `$HOME/Documents/vault-bridge`
- local repo path: `$HOME/code/vault-bridge`

Use stream mode. Stop with `Ctrl+C`. Start again with the same command. The client resumes from the saved cursor under `~/Library/Application Support/vault-bridge/`.

## Preconditions

The macOS machine must have:

- Go installed
- SSH access to the server host used by `rsync`
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
if [ -d vault-bridge/.git ]; then
  cd vault-bridge && git pull --ff-only
else
  git clone https://github.com/NolanHo/vault-bridge.git
  cd vault-bridge
fi
```

## Build the client binary

```bash
cd "$HOME/code/vault-bridge"
go build -o bin/vault-bridge-client ./cmd/vault-bridge-client
```

## One-shot validation

Run this once before the long-lived stream. It validates server connectivity, local path permissions, and transfer mode.

```bash
cd "$HOME/code/vault-bridge"
./bin/vault-bridge-client \
  -server http://server-host:9090 \
  -local-root "$HOME/Documents/vault-bridge" \
  -state-dir "$HOME/Library/Application Support/vault-bridge" \
  -sync-mode auto \
  -rsync-source server-host:/srv/vault-bridge/source/ \
  -rsync-bin /opt/homebrew/bin/rsync \
  -json
```

Expected result:

- if `rsync` works, output JSON includes `"transfer_mode":"rsync"`
- if `rsync` is unavailable or fails, output JSON includes `"transfer_mode":"http"` and a `fallback_reason`

## Foreground stream command

This is the normal persistent foreground mode:

```bash
cd "$HOME/code/vault-bridge"
./bin/vault-bridge-client \
  -stream \
  -server http://server-host:9090 \
  -local-root "$HOME/Documents/vault-bridge" \
  -state-dir "$HOME/Library/Application Support/vault-bridge" \
  -sync-mode auto \
  -rsync-source server-host:/srv/vault-bridge/source/ \
  -rsync-bin /opt/homebrew/bin/rsync \
  -stream-poll-interval 1s \
  -debounce 1s \
  -reconnect 5s
```

Expected terminal behavior:

- startup line: `vault-bridge stream started ...`
- when files change on the server:
  - `HH:MM:SS upsert <relative-path>`
  - `HH:MM:SS delete <relative-path>`
- no output while idle

Stop with:

```bash
Ctrl+C
```

## Alias to add

Add this line to `~/.zshrc` or `~/.bashrc`:

```bash
alias vault-bridge-docs='cd "$HOME/code/vault-bridge" && SERVER=http://server-host:9090 LOCAL_ROOT="$HOME/Documents/vault-bridge" STATE_DIR="$HOME/Library/Application Support/vault-bridge" SYNC_MODE=auto RSYNC_SOURCE=server-host:/srv/vault-bridge/source/ RSYNC_BIN=/opt/homebrew/bin/rsync STREAM=1 ./scripts/run-macos-client.sh'
```

Reload shell config:

```bash
source ~/.zshrc
```

Then run:

```bash
vault-bridge-docs
```

## State and logs

Client state directory:

- `~/Library/Application Support/vault-bridge/cursor`
- `~/Library/Application Support/vault-bridge/client.lock`
- `~/Library/Application Support/vault-bridge/client.log`

Inspect logs:

```bash
tail -f "$HOME/Library/Application Support/vault-bridge/client.log"
```

## Failure diagnosis

If startup fails:

1. verify server health:

```bash
curl http://server-host:9090/healthz
```

2. verify SSH and rsync:

```bash
ssh server-host 'echo ok'
/opt/homebrew/bin/rsync --version
```

3. if `rsync` keeps failing, force HTTP mode to isolate transfer issues:

```bash
cd "$HOME/code/vault-bridge"
./bin/vault-bridge-client \
  -stream \
  -server http://server-host:9090 \
  -local-root "$HOME/Documents/vault-bridge" \
  -state-dir "$HOME/Library/Application Support/vault-bridge" \
  -sync-mode http
```

If HTTP mode works and `auto` does not, the fault is in the `rsync` path or SSH transport, not in the journal stream.
