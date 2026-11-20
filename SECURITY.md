# Security policy

RamPlus runs a root daemon and changes swap and kernel settings, and its web interface can be reached
over the network when explicitly configured. We take security reports seriously.

## Reporting a vulnerability

**Please do not open a public issue, pull request or discussion for a security problem.**

Report it privately through GitHub Security Advisories:

1. Open <https://github.com/ashot0907/RamPlus/security/advisories/new>
   (repository → **Security** → **Report a vulnerability**).
2. Describe the issue, the affected version or commit (`ramplus --version`), and the steps to
   reproduce. A proof of concept helps, but please do not include data from systems you do not own.

We aim to acknowledge reports within 7 days. We will work with you on a fix, agree on a disclosure
date, and credit you in the advisory unless you prefer otherwise.

## Scope

Examples of what we want to hear about:

- privilege escalation through `ramplusd` (the Unix socket API, `SO_PEERCRED` checks, group handling);
- bypassing login, CSRF protection, the `Host` check or rate limiting in `ramplus-web`;
- path handling that lets RamPlus create, overwrite or delete files it should not;
- anything that makes rollback act on state it did not create;
- secrets (password hashes, session tokens) leaking into logs or the world-readable journal.

Out of scope: problems that require root already, or a configuration that the documentation
explicitly warns against (for example exposing the web UI on a network address without a firewall).

## Supported versions

RamPlus is in alpha; only the latest commit on `main` and the latest release receive fixes.

## Hardening notes

How RamPlus protects the system and the web interface is described in [docs/safety.md](docs/safety.md)
and [docs/web.md](docs/web.md#security).
