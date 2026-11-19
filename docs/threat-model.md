# Threat model

This document is for operators and reviewers. It tells what kryostate
protects, from whom, and which protections it does not have. kryostate can
log in to every host, so the computer that runs it is the most valuable
target.

## Assets

| Order | Asset | If an attacker gets it |
|---|---|---|
| 1 | Credential material: keys, passwords, passphrases | The attacker can log in to the hosts. |
| 2 | Connection data: addresses, user names, what runs where | The attacker can plan an attack. |
| 3 | The record: the assertion history, and the applications, stacks and roles that people stated | People make decisions on false data. |

If a design protects the record but discloses credentials, the design has
failed.

## Attackers

| Attacker | Examples |
|---|---|
| A person with a copy of the database file | A backup, a snapshot, a developer laptop |
| A person who can edit a collector or a credential helper | A contributor, a compromised build, an operator who can write the configuration |
| A person who controls a host | A compromised host that answers a collection |
| A person on the network between kryostate and a host | DNS answers, TCP connections |
| A person who can connect to the web interface | Refer to [The web interface](#the-web-interface). |

kryostate gives no protection against a person with privileges on the
computer that runs it. That person can read memory, attach a debugger,
replace the binary or read the output of the credential helper.

## Credentials

The database stores only credential references (`scheme:value`), never
material. kryostate rejects a reference with no scheme. A guess can try a
password as a file path and write it to a log.

| Scheme | Source | Material in the kryostate process |
|---|---|---|
| `agent:` | An ssh-agent socket | None. The agent signs. Use this scheme when possible. |
| `keyring:` | The OS keystore | While in use |
| `file:` | A file, decrypted first if its name ends in `.age` | While in use |
| `exec:` | The standard output of a command | While in use |
| `env:` | An environment variable | For the life of the process |

**WARNING:** Any process that can read the environment of kryostate can read an
`env:` credential. `env:` is the weakest scheme. Use it only if your container
deployment needs it.

### A stolen database file

The file gives assets 2 and 3: host names, addresses, what runs where, the
credential reference of each target and the full assertion history. It does
not give credentials or access to the hosts.

It also gives up to 8 KB of command output for each host and collector spec.
The shipped commands read release files and version strings. Your own
collectors decide what else is stored.

| Task | Command |
|---|---|
| Show the stored output | `kryostate evidence list --show` |
| Delete stored output | `kryostate evidence rm` |
| Do not store output | `kryostate collect --no-evidence` |

With `--no-evidence`, kryostate must connect to every host again after you
correct a parse rule.

### Audit

| Event | Audit row |
|---|---|
| Credential resolution | Action, the reference with its value truncated, and success or failure. It never contains the material, or where the credential is kept. |
| Disable, enable or delete a host | The change and the person. A deletion also records the host name and the reason. |
| Hold or release a credential | The hold or the release |

`kryostate host rm` deletes all data about the host except its audit rows.
Thus, the trail shows what was deleted, by whom and why. No part of
kryostate deletes audit rows.

kryostate does not contact a disabled host: no collection, external
collector, health check, DNS lookup or collector test. Disable a host to stop
credentials going to a host that is possibly not the correct host.

### Account lockout protection

A directory locks an account after some bad passwords. kryostate logs in to
every host, so it can lock its own account quickly after a password change.
Thus, it limits password logins: `keyring:`, `env:`, `exec:` and all WinRM.

| Protection | Effect |
|---|---|
| `engine.connectGuarded` | In a run, targets with a password that has not logged in yet in that run connect one at a time. |
| `collect.passwordAuth` | SSH offers the password one time for each connection, by `password` or keyboard-interactive, never both. |
| WinRM first command | WinRM runs one small command before the collectors, so a bad password fails one time for each host. |
| Credential hold | Two authentication failures in sequence put the credential on hold in the `credential_hold` table. All processes that share the database read it. |

kryostate sends a held credential to no host until a person stores a new
password or runs `kryostate secret release <ref>`. Keys and agents have no
limits, because a refused key does not lock an account.

### Deliberate omissions

- **No cryptography of its own.** Material on disk is in an age-encrypted
  file, read with `filippo.io/age`.
- **Zeroing is best-effort.** `Secret.Zero` overwrites its slice, but Go
  copies and moves memory. This makes a heap dump less useful, but not
  useless.
- **No secret goes through a template.** The web layer has no access to
  resolved material, and no handler resolves a reference.

## Transport

| Rule | Details |
|---|---|
| SSH host keys | kryostate records the key on the first connection (trust on first use, policy `pinned`). A changed key stops the connection with `ErrHostKeyChanged`, with no retry. The `strict` policy refuses a host with no recorded key (`ErrHostKeyUnknown`). No command sets `strict` at this time. |
| WinRM | HTTPS only, port 5986 by default, with certificate verification. `engine.dial` sets HTTPS, and no flag turns it off. The password goes with HTTP Basic authentication inside TLS. If a host has no certificate, kryostate does not collect from it. |
| Timeouts | kryostate never retries a command after a timeout. The remote command can still run, so a retry can run it two times. |
| Output size | Output has a limit before kryostate reads it. Parsers get bounded bytes and hold no credentials. A very large answer is truncated. |
| Host answers | A host decides its own answers, and its values win over a person or a document. kryostate records the collector and keeps the raw bytes, so you can trace a false answer. |

**CAUTION:** The first SSH connection is the weak point. If this is a risk,
record the host keys through a different, trusted channel.

## External collectors

An external collector runs on the computer that runs kryostate, not on the
host. Its environment contains only `KRYOSTATE_TARGET`, `KRYOSTATE_ADDRESS`,
`KRYOSTATE_SCHEMA`, `PATH` and the variables in its own definition. On Windows,
Go also adds `SystemRoot`, because no process can load a DLL without it. A
test makes sure of this boundary. The collector gets no key, password or agent
socket.

**WARNING:** An external collector runs with the privileges of kryostate. The
boundary does not make it safe. Review each external collector before you
install it.

kryostate rejects all of the output if one value is malformed, because the
malformed value shows a bug.

## The web interface

kryostate has no built-in authentication. Your environment already has an
authentication system, so put an authenticating proxy in front of `serve`.

**WARNING:** Without an authenticating proxy, do not listen on an address
other than localhost. All persons who can connect to the port can read every
host, address and credential reference. They can start a collection run and
change the schedule of each collector.

The server records the user name of each change from `X-Forwarded-User`,
`X-Remote-User` or `X-Auth-Request-Preferred-Username`, else `anonymous`.

**WARNING:** The server trusts these headers completely. Use them only behind a
proxy that sets them and removes the client's copies. Without it, a client can
write any user name into the record.

| Area | Protection |
|---|---|
| Cross-site requests | All changes are `POST`, and each `POST` goes through Go `http.CrossOriginProtection`. It refuses a request that a browser marks as from a different site. It is not a token and does not stop other clients. Give proxy origins with `serve --trusted-origin`. |
| Runs | Only one collection run at a time. |
| Schedules | Maximum 4 sessions and 1 for each host. Hosts are spread at random. Backoff for an unreachable host is 10 minutes to 6 hours. The shortest schedule is 5 minutes. Manual and scheduled sessions wait for each other. |
| Queries | Go evaluates them on loaded values. No part goes to SQL. Go regular expressions run in linear time. |
| JSON API `/api/v1` | Read-only: methods other than `GET` get 405. Same port and no login, so the same proxy must protect it. It omits credential references, host keys, notification destinations and passwords. |
| Progress stream | Drops updates to a slow client. It does not block the run. |
| Assets | All are in the binary. The interface sends no requests to the internet. |

### Changes the page cannot make

All persons who can connect to the page can use it. Thus, it cannot run
commands or disclose credentials.

- **No typed commands.** Your own collectors are in a file on the server. The
  page can switch on catalog collectors with compiled-in commands. Template
  values (a path, key, section, variable or package name) must match narrow
  patterns. These do not allow a quote, `$`, backtick, `;`, `|`, `&`, `>`,
  newline or `..`. The value goes in single quotes, where `sh` and PowerShell
  expand nothing. Tests send injection attempts to each pattern.
- **No content templates by default.** File contents, configuration settings
  and environment variables can contain secrets. The page offers them only
  with `serve --allow-content-collectors`. The command line always can.
- **Only `agent:` targets with no bastion.** Otherwise, a person could change
  the address of a password target and get the password.
- **No host key reset.** Use `kryostate target forget-host-key`. The old key
  goes to the audit log.
- **No collector changes.** The page shows your collector commands, and the
  path, arguments and environment variable names of external programs. It
  shows environment variable values as `(hidden)`.

**WARNING:** All persons who can connect to the page can read the commands in
`collectors.json`. Do not put secrets in them.

## Separation of duties

By default, `serve` runs the schedules and **Collect now**, so it needs the
ssh-agent. To keep credentials out of the web process:

1. Start `kryostate serve --no-schedule` as an OS user with no credentials.
2. Run `kryostate collect` or `kryostate refresh` on a timer as an OS user
   with the credentials.

Then **Collect now** cannot log in to a host. kryostate permits this
separation but does not enforce it. Both commands are in one binary and use
one database.

## Known gaps

- No authentication or authorization. A proxy must supply them. Cross-site
  protection works only for browsers that send `Sec-Fetch-Site` or `Origin`.
- No encryption at rest. Use file permissions and disk encryption.
- Trust on first use for SSH host keys. After `target forget-host-key`,
  nothing makes sure that the person compared the new fingerprint.
- Audit rows are in the same database. An attacker who can write it can
  change them.
- `env:` credentials stay in memory for the life of the process.
- No rate limit on web requests. A person who can connect can start full
  runs, one at a time, without a limit.
- With Go 1.27.1, `govulncheck` reports fourteen standard library findings.
  Two are escaping bugs in `html/template` (GO-2026-6600, GO-2026-6599), fixed
  in Go 1.27.2. Build with a current toolchain and run `govulncheck ./...`.
  The `go` directive stays at 1.27.1 only so that older toolchains can build
  the project.
