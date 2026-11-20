# Daemon API (ramplusd)

`ramplusd` is a root daemon that exposes RamPlus over HTTP/JSON on a **Unix socket**
`/run/ramplus/ramplusd.sock`. It does not listen on TCP. It is a thin layer over the same
`internal/core` as the CLI: plans, checks and execution are identical.

The web UI (`ramplus-web`) is one client of this API; you can also use `curl` or your own scripts.

## Running

```sh
make build && sudo make install              # binaries in /usr/local/bin, units, sysusers.d, tmpfiles.d
sudo systemctl enable --now ramplusd
sudo usermod -aG ramplus $USER               # read access: status, history, plans
sudo usermod -aG ramplus,ramplus-admin $USER # and changes: apply
ramplus daemon status                        # reachable? same version as the CLI?
curl --unix-socket /run/ramplus/ramplusd.sock http://localhost/v1/status
```

(Log out and back in for new group membership to apply.)

| flag | default | |
|---|---|---|
| `--socket` | `/run/ramplus/ramplusd.sock` | socket path |
| `--group` | `ramplus` | read access; group owner of the socket |
| `--admin-group` | `ramplus-admin` | change access (apply) |
| `--web-user` | `ramplus-web` | operations requested by this user are recorded as "via the web interface" |
| `--version` | | print the version |

## Permissions

- The socket is `0660 root:ramplus` inside `/run/ramplus` (`0750 root:ramplus`): without the
  `ramplus` group a process cannot even reach it.
- Every connection is identified by `SO_PEERCRED` (uid, gid and pid from the kernel); supplementary
  groups are read from `/proc/<pid>/status`.
- **Read** (`status`, `history`, `recommendation`, `profiles`, all `plan/*`, `operations`): root or
  group `ramplus` (or `ramplus-admin`).
- **Change** (`apply`): root or group `ramplus-admin`.
- `GET /v1/health` is open to anyone who reached the socket.
- A refusal is `403 auth.forbidden` plus a journal record (see [Logs](#logs)).
- If a group does not exist, only root has that access.

## Endpoints

| endpoint | access | |
|---|---|---|
| `GET /v1/health` | any | version, uptime, pid, whether the daemon runs as root |
| `GET /v1/status` | read | same as `ramplus status --json` |
| `GET /v1/status/stream` | read | SSE: a status snapshot every second plus peaks |
| `GET /v1/history` | read | same as `ramplus history --json` |
| `GET /v1/recommendation` | read | same as `ramplus recommend --json`; `?path=` evaluates another swapfile location |
| `GET /v1/profiles` | read | profile catalogue, kernel parameters and their current values |
| `POST /v1/plan/setup` | read | `{"swap": {"size", "existing", "delete_old", "path", "force"} \| null, "profile": {"name", "set", "experimental", "force"} \| null}` |
| `POST /v1/plan/swap` | read | `{"action": "apply"\|"resize"\|"remove", "size": "4G", "existing", "delete_old", "path", "force", "orphan"}` |
| `POST /v1/plan/profile` | read | `{"name", "set": {"vm.swappiness": "80"}, "experimental", "force"}` |
| `POST /v1/plan/profile-reset` | read | no body |
| `POST /v1/plan/rollback` | read | optional `{"force": true}` |
| `POST /v1/apply` | change | `{"plan_id", "confirm_experimental"}` → `202 {operation_id}` |
| `GET /v1/operations/{id}` | read | operation status, steps so far, result |
| `GET /v1/operations/{id}/events` | read | SSE: steps as they run (`step`), then the result (`result`) |

`size` is either a string with units (`"4G"`, `"512M"`) or a number of bytes. Request bodies are JSON,
at most 64 KiB, unknown fields are rejected. Plans use the [plan schema](cli.md#json-output).

### Example

```sh
S=/run/ramplus/ramplusd.sock
curl -s --unix-socket $S http://localhost/v1/recommendation

curl -s --unix-socket $S -X POST http://localhost/v1/plan/setup \
  -H 'Content-Type: application/json' \
  -d '{"swap": {"size": "16G", "existing": "replace"}, "profile": {"name": "zswap-lz4"}}'
# → plan with "id": "…", "blocked": false, checks, steps

curl -s --unix-socket $S -X POST http://localhost/v1/apply \
  -H 'Content-Type: application/json' \
  -d '{"plan_id": "<id>", "confirm_experimental": true}'
# → 202 {"operation_id": "…", "status": "running", "self": "/v1/operations/…", "events": "…/events"}

curl -N --unix-socket $S http://localhost/v1/operations/<operation_id>/events
```

## Asynchronous apply

`POST /v1/apply` is asynchronous. Everything that can fail **without changing anything** is checked
synchronously:

1. the plan exists and has not been applied yet;
2. the lock is free (no other RamPlus process is working);
3. the facts are gathered again under the lock and the plan fingerprint is unchanged;
4. all checks pass;
5. an experimental profile is confirmed (`confirm_experimental`).

Only after the operation is written to the journal does the daemon answer `202`; the steps run in the
background. If there is nothing to do, the answer is `200` with `"status": "nothing"`.

- One operation at a time: a second `apply` gets `409 op.busy`.
- A plan can be applied once (`409 plan.used`) and lives 15 minutes; the daemon keeps the last 64
  plans and the last 32 operations in memory. After a restart an old `plan_id` is `404 plan.not_found`
  (the journal, of course, survives).
- The operation does not depend on the HTTP request: a client disconnect does not interrupt it, and on
  `SIGTERM` the daemon finishes the current operation before exiting.

## Server-sent events

`/v1/status/stream`:

```
event: status
data: {"status": {…same as /v1/status…}, "peaks": {"pressure_level": "normal", "psi_some_avg10": 0.4, …}}
```

`/v1/operations/{id}/events` first replays the steps already done, then streams new ones, and closes
after the result:

```
event: step
data: {"undo": false, "index": 3, "total": 12, "step": {"kind": "mkswap", "path": "/ramplus.swapfile", "description": "…"}}

event: result
data: {"status": "done", "operation_id": "…", "events": […]}
```

A comment line `: ping` is sent every 15 s to keep the connection alive. Steps with `"undo": true`
are the automatic rollback after a failed step.

## Errors

Errors are `{"code", "message"}` with a stable `code`:

| HTTP | code | meaning |
|---|---|---|
| 400 | `request.invalid` | bad JSON, unknown field, bad value |
| 403 | `auth.forbidden` | no permission (see above) |
| 404 | `request.not_found` | no such endpoint |
| 404 | `plan.not_found` | unknown or expired `plan_id` |
| 404 | `op.not_found` | unknown operation (the daemon only knows operations it started) |
| 405 | `request.method_not_allowed` | wrong method |
| 409 | `plan.used` | the plan was already applied |
| 409 | `plan.stale` | the system changed since the plan was built — build it again |
| 409 | `op.busy` | another operation is running in the daemon |
| 409 | `op.locked` | another RamPlus process (for example the CLI) holds the lock |
| 409 | `op.cancelled` | cancelled |
| 413 | `request.too_large` | body over 64 KiB |
| 422 | `plan.blocked` | a check failed; the body includes the `plan` |
| 422 | `profile.experimental_not_confirmed` | experimental profile without `confirm_experimental` |
| 500 | `daemon.not_root` | the daemon is not running as root, it cannot apply |
| 500 | `journal.unreadable` | `/var/lib/ramplus/state.json` cannot be read |

A plan with failed checks is **not** an HTTP error: `POST /v1/plan/*` returns `200` with
`"blocked": true`.

### Paths the daemon cannot write

The daemon runs with `ProtectHome=read-only`, so it cannot create a swapfile under `/home`. Such paths
are blocked at plan time with `path.not_writable_by_daemon` ("not available through the web
interface/daemon, use the CLI") instead of failing halfway through.

## Logs

The daemon logs to journald in `key=value` format. Every record has a stable machine field `event=`;
the `msg` text may change.

| event | fields |
|---|---|
| `request` | `method`, `path`, `status`, `uid`, `pid`, `ms` |
| `auth.forbidden` | `uid`, `gid`, `gids` (comma-separated, `none` if empty), `pid`, `method`, `path`, `need` (`read`/`admin`) |
| `operation.started` | `id`, `plan_id`, `command`, `uid`, `user`, `via`, `pid` |

```sh
journalctl -u ramplusd -o cat | grep -w 'event=auth.forbidden'
```

## Reliability

- `mlockall`: the daemon is needed exactly when memory is short and must not be swapped out.
- `GOMEMLIMIT=64MiB`, `OOMScoreAdjust=-900`.
- On `SIGTERM` it closes streams, waits for the current operation and exits.
- The choice of systemd hardening options, with reasons for each one enabled or not, is in the
  comments of [packaging/systemd/ramplusd.service](../packaging/systemd/ramplusd.service).
