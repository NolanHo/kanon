# Linux supervisord deployment guide

This guide documents how to deploy `vault-bridge-server` on hosts where process lifecycle is managed by `supervisord`.

It covers the exact pitfalls hit during real deployment on this host.

## Deployment model

On this host, `vault-bridge-server` is not managed by `systemd --user`.

- Supervisor main config: `/mlplatform/supervisord/supervisord.conf`
- Program config: `/mlplatform/supervisord/conf.d/vault-bridge-server.conf`
- Start script: `/vePFS-Mindverse/user/nolanho/code/vault-bridge/scripts/run-linux-server.sh`

Program config currently sets:

- `BUILD_ON_RUN="0"`
- `CONFIG_FILE="/vePFS-Mindverse/user/nolanho/code/vault-bridge/config/filter.json"`
- `ROOT="/vePFS-Mindverse/user/nolanho/docs"`

`BUILD_ON_RUN=0` means restart does not rebuild binaries.

## Release procedure

Run from repo root:

```bash
cd /vePFS-Mindverse/user/nolanho/code/vault-bridge
```

1) Validate and build

```bash
go test ./...
go build -o bin/vault-bridge-server ./cmd/vault-bridge-server
go build -o bin/vault-bridge-client ./cmd/vault-bridge-client
```

2) Restart via supervisord

If `supervisorctl` is in PATH:

```bash
supervisorctl -c /mlplatform/supervisord/supervisord.conf restart vault-bridge-server
```

If not in PATH (common on this host), use the Nix binary directly:

```bash
/nix/store/5mc40v8qa34jyilh5jgsfi1sc42f77hv-python3.8-supervisor-4.2.2/bin/supervisorctl \
  -c /mlplatform/supervisord/supervisord.conf restart vault-bridge-server
```

3) Verify running state

```bash
/nix/store/5mc40v8qa34jyilh5jgsfi1sc42f77hv-python3.8-supervisor-4.2.2/bin/supervisorctl \
  -c /mlplatform/supervisord/supervisord.conf status vault-bridge-server

ps -eo pid,etimes,%cpu,%mem,cmd | grep -E 'vault-bridge-server .*:39090' | grep -v grep
```

4) Verify API health

```bash
curl -sS http://127.0.0.1:39090/healthz
curl -sS 'http://127.0.0.1:39090/v1/changes?since=0&limit=2'
```

## Logs

Supervisor logs configured for this host:

- stdout: `/vePFS-Mindverse/user/nolanho/.local/state/vault-bridge/server.supervisor.stdout.log`
- stderr: `/vePFS-Mindverse/user/nolanho/.local/state/vault-bridge/server.supervisor.stderr.log`

Read last lines:

```bash
tail -n 80 /vePFS-Mindverse/user/nolanho/.local/state/vault-bridge/server.supervisor.stderr.log
tail -n 80 /vePFS-Mindverse/user/nolanho/.local/state/vault-bridge/server.supervisor.stdout.log
```

## Failure modes and root causes

### 1) Tried `systemctl --user`, service looked missing

Root cause: this host uses `supervisord`, not user `systemd` for `vault-bridge-server`.

Fix: use `supervisorctl` with `/mlplatform/supervisord/supervisord.conf`.

### 2) `supervisorctl: command not found`

Root cause: supervisor tools are installed under Nix store and not exported into PATH.

Fix: call the full binary path under `/nix/store/.../bin/supervisorctl`.

### 3) Restart succeeds but code is stale

Root cause: `BUILD_ON_RUN=0`, so restart reuses existing binaries.

Fix: always run `go build` before `supervisorctl restart`.

### 4) Log shows `go: command not found`

Root cause: running with `BUILD_ON_RUN=1` requires Go in supervisor runtime PATH, which is often absent.

Fix: keep `BUILD_ON_RUN=0` in production and build explicitly during release.
