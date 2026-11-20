# Web UI (ramplus-web)

`ramplus-web` serves the built-in web interface (React + TypeScript, compiled into the binary) and
proxies `/api/*` to the daemon socket (`/v1/*`) using an explicit allow-list of paths. It never runs
as root: it runs as the system user `ramplus-web`, which is in the `ramplus` and `ramplus-admin`
groups. It writes nothing to disk.

## Install and open

```sh
make build && sudo make install
sudo ramplus init                     # set the password (or --password-stdin in scripts)
sudo systemctl enable --now ramplusd ramplus-web
xdg-open http://127.0.0.1:7781
```

Password management:

```sh
sudo ramplus passwd             # change; open sessions end immediately
sudo ramplus passwd --disable   # remove; the UI shows only "run sudo ramplus init"
```

The password is picked up without restarting `ramplus-web`.

## On a server: SSH tunnel

By default the web UI listens only on `127.0.0.1:7781`. From your own computer:

```sh
ssh -L 7781:localhost:7781 user@server
# then open http://127.0.0.1:7781 locally
```

This is the recommended way: no open port, SSH does the authentication and encryption.

## Listening on a network address (TLS required)

Any address other than loopback requires TLS; without it `ramplus-web` refuses to start.

| flag | default | |
|---|---|---|
| `--listen` | `127.0.0.1:7781` | address:port |
| `--tls-self-signed` | | TLS with a self-signed certificate generated at start |
| `--tls-cert`, `--tls-key` | | TLS certificate and key (PEM); both are required |
| `--host` | | extra allowed `Host` name, repeatable (see [Host check](#host-check)) |
| `--socket` | `/run/ramplus/ramplusd.sock` | daemon socket |
| `--auth-file` | `/etc/ramplus/web-auth` | password file |
| `--version` | | print the version |

Change the command line with a systemd drop-in (the empty `ExecStart=` clears the original one):

```sh
sudo systemctl edit ramplus-web
```

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/ramplus-web --listen 0.0.0.0:7781 --tls-self-signed --host server.lan
```

```sh
sudo systemctl restart ramplus-web
journalctl -u ramplus-web | grep sha256   # self-signed: compare this fingerprint in the browser
```

Notes:

- A self-signed certificate is generated on every start, so its fingerprint changes after each
  restart. For permanent use, give your own certificate with `--tls-cert`/`--tls-key`.
- The service runs with `ProtectSystem=strict` and `ProtectHome=yes`: certificate files must be
  outside `/home` and readable by `ramplus-web`, for example
  `/etc/ramplus/tls/{cert,key}.pem` owned `root:ramplus-web`, mode 0640.
- A specific `--listen` IP is allowed as a `Host` automatically; for `0.0.0.0` add the names and
  addresses you will type in the browser with `--host`.
- Consider a firewall rule limiting who can reach the port.

## Security

**Password.** argon2id hash (64 MiB, t=3, p=2) in `/etc/ramplus/web-auth` (`root:ramplus-web`,
0640). Set only by `sudo ramplus init`. At most two password checks run at a time.

**Sessions.** On login the server creates a random session id stored in memory and sends it in a
cookie: `HttpOnly`, `SameSite=Strict`, `Path=/`, lifetime 12 hours; under TLS also `Secure` with the
`__Host-` prefix. Logout deletes the session. Changing the password ends all sessions. Sessions do not
survive a restart of `ramplus-web`.

**CSRF.** Every state-changing request (`POST`) must carry the session's token in the `X-CSRF-Token`
header. Additionally the `Origin` (if present) must be an allowed host and `Sec-Fetch-Site` must not be
`cross-site`. Login requires `Content-Type: application/json`, which a cross-site form cannot send
without a preflight.

**Login rate limit.** 5 failed logins within 5 minutes from one address → a 30-second pause; each
following pause doubles, up to 15 minutes. During a pause even the correct password is refused
(`429 auth.rate_limited`, `Retry-After`). Failed logins are logged to journald, passwords never are.

<a id="host-check"></a>**Host check (DNS rebinding).** Allowed `Host` values: `127.0.0.1`,
`localhost`, `[::1]` (any port) and names given with `--host`. Anything else gets
`421 host.not_allowed`.

**Headers.** On every response:

```
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self';
  font-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self';
  frame-ancestors 'none'
X-Frame-Options: DENY
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Resource-Policy: same-origin
```

No inline scripts, no external requests: the Inter and Manrope fonts (with Cyrillic) are bundled.

**Privileges.** `ramplus-web` runs as an unprivileged user with an empty capability set, a read-only
filesystem and a syscall filter. Each hardening option is explained in
[packaging/systemd/ramplus-web.service](../packaging/systemd/ramplus-web.service). Operations it
requests are recorded in the journal with `source: "daemon"` and `via: "web"` — the daemon recognises
the `ramplus-web` user by `SO_PEERCRED`, a header cannot fake it.

**No password set** → the UI shows only "run `sudo ramplus init`", and every API call returns
`503 auth.not_configured`.

## API through the web server

Besides the proxied daemon endpoints (`/api/X` → `/v1/X`, see [api.md](api.md)), `ramplus-web` has its
own:

| endpoint | |
|---|---|
| `GET /api/session` | `{password_set, authenticated, csrf_token, expires_at, version}` |
| `POST /api/login` | `{"password": "…"}` → session cookie and `csrf_token`; `401 auth.invalid` with `attempts_left`; `429 auth.rate_limited` with `retry_after_seconds` |
| `POST /api/logout` | needs the CSRF token; `204` |

Proxied (everything else under `/api/` is `404`): `GET health, status, status/stream, history,
recommendation, profiles, operations/{id}, operations/{id}/events`; `POST plan/setup, plan/swap,
plan/profile, plan/profile-reset, plan/rollback, apply`. SSE streams are passed through without
buffering. Cookies and the CSRF header are not forwarded to the daemon.

Web-server errors: `401 auth.required`, `403 csrf.invalid`, `403 origin.forbidden`,
`421 host.not_allowed`, `502 daemon.unavailable` (the daemon is not running),
`500 auth.unreadable` (the password file cannot be read — is `ramplus-web` in the `ramplus-web`
group?).

## Logs

`journalctl -u ramplus-web -o cat`, with a stable `event=` field: `request`, `login.ok`,
`login.failed` (`ip`, `attempts_left`, `lockout_seconds`), `login.rate_limited`, `csrf.invalid`,
`host.rejected`, `daemon.unavailable`, `auth.unreadable`.

## Languages

English and Russian; the default follows the browser language, the switch is in the header. Plan checks
are translated by their stable `code`; if a code has no translation, the server's message is shown.

## Troubleshooting

| symptom | what to do |
|---|---|
| "RamPlus daemon is unavailable" | `sudo systemctl start ramplusd`; `systemctl status ramplusd` |
| "Set a password" page | `sudo ramplus init` |
| "No permission to change settings" | `ramplus-web` must be in `ramplus-admin`: `id ramplus-web`, then restart `ramplus-web` |
| page "frontend is not built" | the binary was built without `make web`; run `make build` |
| plan blocked with "not available through the web interface" | the swapfile path is under `/home`; use the CLI |

## Uninstall

`sudo make uninstall PURGE=1` also removes the `ramplus-web` user and `/etc/ramplus/web-auth`.
Without `PURGE=1` both are kept.
