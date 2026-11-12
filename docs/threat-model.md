# Threat model

This document exists because the tool holds the means to log in to every host
it describes. That makes the machine running it worth more to an attacker than
any single machine in the inventory, and it is worth saying so before making
any claim about how credentials are handled.

It describes the design as built. Where a protection is absent, that is stated
rather than left for a reader to assume.

## What is being protected

1. **Credential material** — keys, passwords and passphrases used to reach
   hosts. Compromise means the estate, not the inventory.
2. **Connection detail** — addresses, usernames, which host runs what. Useful
   to someone planning an attack even without credentials.
3. **The record itself** — the assertion history and the decisions recorded
   against discrepancies. Compromise means decisions get made on fabricated
   evidence.

The order matters. A design that protects the record but leaks credentials has
failed.

## Who is being defended against

| | |
|---|---|
| Someone who obtains a copy of the database file | A backup tape, a snapshot, a developer laptop |
| Someone who can edit a collector or a credential helper | A contributor, a compromised build, an operator with write access to the configuration |
| Someone who controls a target host | A compromised server answering a collection |
| Someone on the network between the tool and a target | DNS answers, TCP connections |
| Someone who can reach the web interface | See the section on authentication below |

**Not defended against:** anyone with privileges on the host running the tool.
They can read process memory, attach a debugger, replace the binary, or read
whatever the credential helper returns. No design in this tool prevents that.

## Credentials

The database stores *references*, never material. A reference is
`scheme:value`:

| Scheme | Resolves to | Material in this process |
|---|---|---|
| `agent:` | A handle to an ssh-agent socket | None — the agent signs |
| `keyring:` | An entry in the OS keystore | While in use |
| `file:` | A file, decrypted first if it ends `.age` | While in use |
| `exec:` | The standard output of a command | While in use |
| `env:` | An environment variable | For the process lifetime |

`agent:` is the one to prefer, because no key enters this process at any point.
`env:` is the weakest and is offered because container deployments expect it;
anything that can read the process environment reads the credential, and it
lives in memory for as long as the process does.

A reference without a scheme is rejected rather than guessed at. Guessing is
how a password ends up being tried as a file path and written to a log.

### What a stolen database file gives an attacker

Everything in categories 2 and 3 above: hostnames, addresses, what runs where,
which credential reference each host uses, and the full assertion history. It
does not give them credentials. Theft of the file leaks the inventory. It does
not hand over access to the hosts the inventory describes.

It also gives them up to 8 KB of output per host and collector spec.
The shipped commands read release files and version strings, but a site that
adds its own decides what ends up there. `hearsay evidence -show` is how you
find out, `-forget` is how you remove it, and `collect -no-evidence` is how you
never store it. The cost of that last option is that fixing a parse rule then
requires connecting to every host again.

### What is audited

Every credential resolution writes an audit row naming the reference with its
value truncated, the action, and whether it succeeded. The material is never
written. The row records which credential was used. It does not record where
that credential is kept.

### Deliberate omissions

- **No cryptography is implemented here.** Where material must rest on disk it
  is held in an age-encrypted file using `filippo.io/age`. This project does
  not implement its own cipher.
- **Zeroing is best-effort.** `Secret.Zero` overwrites the slice it holds. Go
  copies and moves memory, so this narrows the window in which a heap dump is
  useful and does not close it.
- **Secrets never pass through a template.** The web layer has no access to
  resolved material, and no handler resolves a reference.

## Transport

**SSH host keys are pinned on first connection.** A changed key aborts the
connection with `ErrHostKeyChanged` and does not retry. The first connection is
trust-on-first-use and is therefore the weak moment; pin keys out of band if
that matters to you.

**WinRM is used over HTTPS only.** `engine.dial` sets HTTPS and there is no
flag to turn it off, because a Negotiate exchange over plain HTTP on a
collection run that touches every Windows host in the estate is not a trade
worth exposing. An estate that cannot present a certificate on 5986 is one this
tool declines to collect from rather than one it quietly downgrades for.

**A timed-out command is never retried.** A local timeout means this process
stopped waiting; it does not mean the remote command stopped running. Retrying
would run it twice.

**Collector output is bounded** before it is read, and parsers receive bounded
bytes and hold no credentials. A compromised host that answers with four
gigabytes of output gets truncated, not obeyed.

**A target decides its own answers.** Everything a host reports is attributed
to the collector that asked, with the raw bytes kept, so a lying host produces
a visible disagreement rather than a silent correction.

## External collectors

An operator-supplied collector runs **on the machine hosting this tool**, not
on the target. Its environment holds `HEARSAY_TARGET`, `HEARSAY_ADDRESS`,
`HEARSAY_SCHEMA`, `PATH`, and any variables its own definition lists — nothing
inherited from this process. On Windows the Go runtime also passes
`SystemRoot`, which cannot be suppressed: a process without it cannot load a
DLL. A test asserts this boundary rather than assuming it.

It is given no key, no password and no agent socket; a collector that needs to
log in somewhere brings its own credential.

This boundary stops a contributed script from borrowing the keys to the estate.
It does **not** make installing one safe: the program runs with this process's
privileges, and installing one is a privileged act. Review them the way you
would review anything else that runs as that user.

Its output is validated as a whole and rejected as a whole. Ten good facts and
one malformed one is a bug, and taking the ten hides it.

## The web interface

**There is no authentication built in.** This is a deliberate decision, and
the reasoning is that an authentication system is the wrong thing for one
project to invent badly when every environment that would run this already has
one.

The consequence: **binding the server to anything other than localhost without
an authenticating proxy in front of it publishes your inventory.** Anyone who
can reach the port can read every host, every address, and every credential
*reference* — and can trigger a collection run against the estate.

The server reads `X-Forwarded-User`, `X-Remote-User` or
`X-Auth-Request-Preferred-Username` to attribute decisions, falling back to
`anonymous`. **These headers are trusted completely.** They are only meaningful
behind a proxy that sets them and strips any the client sent. Directly exposed,
the attribution on every recorded decision is whatever the client typed.

Other properties worth knowing:

- All state-changing operations are `POST`, and every `POST` passes Go's
  `http.CrossOriginProtection`: a request a browser marks as coming from
  another site is refused, so another page cannot submit forms through a
  visitor's browser. It is not a token, and it does nothing about a client
  that is not a browser. Origins a proxy serves the page under are listed with
  `serve -trusted-origin`.
- A second collection run cannot start while one is in progress, so clicking
  twice does not open two waves of sessions into the estate.
- The progress stream drops updates to a slow client rather than blocking a
  collection run.
- Every asset ships in the binary. The interface makes no request to the
  internet, which matters in an estate that has no route to one.

### What the page can change, and what it cannot

Because anyone who reaches the page can use it, the page is limited to changes
that cannot turn it into a way to run commands or to take credentials.

- **No command can be typed into it.** Collectors written by hand live in a
  file on the server. The page can switch on catalog collectors, whose
  commands are compiled in. A catalog template takes values (a path, a key, a
  section, a variable or package name) that must match narrow patterns with
  no quote, `$`, backtick, `;`, `|`, `&`, `>`, newline or `..`, and are placed
  inside single quotes, where neither `sh` nor PowerShell expands anything.
  Tests feed each pattern injection attempts.
- **Templates that read content are off by default on the page.** File
  contents, a setting from a config file and an environment variable can
  return secrets (`pass4SymmKey` sits in Splunk's `server.conf`). The page
  offers them only under `serve -allow-content-collectors`. The command line
  always can, because whoever runs it already has the database.
- **Connections it creates or edits use `agent:` only, without a bastion.** A
  page that could change a password-bearing connection's address could send
  that password to a machine of the attacker's choosing.
- **It cannot clear a pinned host key.** `hearsay target -forget-host-key`
  does that, on the command line, and the old key is written to the audit
  log. Otherwise a visitor could clear a pin and let an impersonating host
  answer the next collection.

## Separation of duties

The web process does not need fleet credentials and the credential-bearing
collector does not need to listen on a port. Running `serve` and `collect` as
different OS users achieves that split, and it is what is recommended.

**The design permits that separation; it does not enforce it.** Both are
subcommands of one binary against one database, and nothing stops an operator
running `serve` as the same user that holds the credentials. Enforcing it would
mean two binaries and a protocol between them, which has not been built.

## Known gaps

These are the weaknesses known at the time of writing.

- No authentication and no authorisation. Both are delegated to a proxy that
  may not be there. Cross-site form submission is refused, but only for
  browsers that send `Sec-Fetch-Site` or `Origin`.
- The database is not encrypted at rest. File permissions and disk encryption
  are the control.
- Trust-on-first-use for SSH host keys. A changed key stops collection from
  that host, with the new key's fingerprint in the error, until someone runs
  `target -forget-host-key`; nothing checks that they compared it first.
- Audit rows live in the same database as the data they describe, so an
  attacker who can write to it can edit them.
- `env:` credential references sit in process memory for the process lifetime.
- No rate limiting anywhere.
- `govulncheck` reports fourteen findings in the Go standard library when
  built with Go 1.27.1, two of them escaping bugs in `html/template`
  (GO-2026-6600 and GO-2026-6599). They are fixed in 1.27.2. Build with a
  current toolchain and run `govulncheck ./...` yourself; the `go` directive is
  left at 1.27.1 only so that the project still builds on an unpatched one,
  which is a tradeoff and not a judgement that the findings do not matter.
