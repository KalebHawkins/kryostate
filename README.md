# hearsay

hearsay keeps a record of servers, logs in to them over SSH and WinRM to ask
what they actually are, and shows you where the two disagree. The list of hosts
is a byproduct. The disagreements are the product. An inventory is hearsay
until a machine confirms it.

It was written because the inventory it replaced was a set of Markdown tables,
generated once by a program nobody kept, hand-edited for two years afterwards,
and wrong in ways that were impossible to find and impossible to date.

## What it does not do

Worth knowing before you read any further:

- **It does not discover anything.** You tell it which hosts exist. It has no
  network scanner, and a machine nobody told it about stays invisible.
- **It has no authentication.** Put an authenticating proxy in front of it, or
  keep it on localhost. Read [docs/threat-model.md](docs/threat-model.md)
  before you bind it to an interface.
- **It does not change anything on your hosts.** It runs read-only commands.
  It is not a configuration tool and will not become one.
- **It does not replace monitoring.** It records probe results. It does not
  page anyone.
- **It is not a dependency map.** Hosts group into stacks because you put them
  there, not because a packet flowed.

## How it works

Everything is an **assertion**: a value, an attribute, a source, a timestamp.
A document saying a host runs one release and the host saying it runs another
are both recorded. Neither overwrites the other. Which one is shown is decided
by per-attribute precedence, and the losing one stays queryable.

A **collector** runs a read-only command and returns bytes. It does not
interpret them. Two layers of repair sit above it, and both work against the
database rather than against your estate:

- **Parsing** takes a substring out of a command's output. The output itself is
  kept, so a rule that read the wrong line is corrected with `reparse`.
- **Normalisation** turns that substring into something comparable, driven by
  rules in a table. A rule that read a version wrongly is corrected with
  `renormalize`.

Neither needs a host to be reachable, which matters most when the host is gone.

**Identity** is an opaque ID. A hostname is evidence, not a key. Names repeat,
get reused, resolve to two addresses or to none; a tool that keys on them is
confidently wrong, which is worse than being empty.

There is **no "last seen" column**. Probes are discrete and kept apart: TCP
connect, SSH authentication, WinRM authentication, collector run, name
resolution, health check. A load balancer answering on port 443 while
everything behind it is down is a thing you want to see, not a thing to average
into a green dot.

ICMP is **not** sent. Raw sockets need a privilege this tool should not hold,
and an echo reply proves less than a connection to the port the host exists to
serve. `hearsay check` opens the port or fetches the URL you name. The schema
reserves a probe kind for ICMP results produced by something that already has
the privilege, but no command accepts them yet.

**Unknown is a visible bucket**, and it has three causes that are kept apart:
never collected, collection failed, and answer not understood. The third is a
bug report about this tool. Charts add up to the number of hosts you have.

See [docs/adr/](docs/adr/) for the reasoning behind each of these.

## Attributes

The core set — hostname, application, role, stack, OS version, application
version, middleware name and version — are ordinary namespaced attributes
(`os.version`, `app.name`, `app.version`, `role`, `site`, `env`), not columns.
Adding one your estate cares about needs no schema change and no code:

```sh
# a collector spec: a command, a parser, an attribute
hearsay collect -collectors ./my-collectors.json

# or an external program that prints an envelope on stdout
hearsay collect -externals ./my-externals.json
```

An external collector runs on the machine hosting hearsay, once per host, with
an environment holding `HEARSAY_TARGET`, `HEARSAY_ADDRESS`, `HEARSAY_SCHEMA`,
`PATH`, and whatever its own definition lists — nothing else. On Windows the Go
runtime also passes `SystemRoot`, because a process without it cannot load a
DLL. It is given no credential. It prints JSON:

```json
{
  "schema": "hearsay.facts/v1",
  "collector": "example",
  "status": "ok",
  "facts": [
    {"key": "product.home", "value": "/opt/product", "type": "path"},
    {"key": "product.version", "value": "4.2.1", "type": "version", "scope": "instance-a"}
  ]
}
```

The envelope is validated and accepted or rejected as a whole. Ten good facts
and one malformed one is a bug, and taking the ten hides it until somebody acts
on the eleventh.

## Getting started

Requires Go 1.27 or later. There is no cgo, no build step, and no runtime
dependency. Build with 1.27.2 or later if you can: earlier 1.27.x is missing
`html/template` escaping fixes you want.

```sh
go build ./cmd/hearsay

# record some hosts
./hearsay host -name appnode1 -fqdn appnode1.example.invalid
./hearsay target -ci <id> -address appnode1.example.invalid -transport ssh \
    -user svc-ro -credential agent:

# or import what you already have written down
./hearsay import -name inventory.md -map ./mapping.json inventory.md

# find out what they really are
./hearsay collect
./hearsay detect

# ask whether they are answering
./hearsay check -ci <id> -name https -http https://appnode1.example.invalid/health
./hearsay check -run

# read the result
./hearsay serve -addr 127.0.0.1:8099
```

`hearsay -h` lists every subcommand. The ones worth knowing early:

| | |
|---|---|
| `import` | Record what a document claims, as claims |
| `collect` | Ask the hosts, over SSH or WinRM |
| `check` | Define and run liveness checks: a TCP connect or an HTTP fetch |
| `resolve` | Record what DNS says, without choosing an address |
| `detect` | Find the disagreements |
| `reparse` | Re-run collector parse rules over stored command output |
| `renormalize` | Re-run normaliser rules over values already collected |
| `evidence` | List or delete the stored output of collector commands |
| `host -retire` | Mark a host gone without losing what it said |
| `serve` | The web interface |
| `export` / `restore` | Whole-database JSON, round-trip tested |

## Credentials

Each host can use a different credential. The database stores a *reference*,
never key material:

| Reference | Resolves through |
|---|---|
| `agent:` | ssh-agent — no key enters this process |
| `keyring:service/account` | The OS credential store |
| `file:/path/to/key` or `file:/path/key.age` | Disk, decrypted with age if encrypted |
| `exec:/path/to/helper arg` | Your existing broker |
| `env:VAR_NAME` | An environment variable — the weakest option |

`agent:` is the one to prefer. A stolen database file should be an inventory
leak, not an estate compromise.

`hearsay secret -set service/account` reads the value from standard input, not
from a flag, so it stays out of your shell history and out of the process list.

## Stored command output

Collection keeps up to 8 KB of what each command printed, replacing the
previous copy for that host and collector. The key is the collector spec, not
the command text, so two specs running the same command keep a copy each. This
is what makes `reparse` possible without connecting to anything.

Output is kept only from commands that ran and exited zero, including when the
parse rule then found nothing in it — that case is exactly what the repair path
is for. Output from a command that failed is discarded, because replaying a
rule over an error message would turn it into a value.

The commands are read-only version and release queries, but they run on your
hosts and you should know what their output contains before it sits in a file.
`hearsay evidence -show` prints it. `hearsay evidence -forget` deletes it, and
`collect -no-evidence` never stores it — at the cost of needing a second pass
over the estate to fix a parse rule.

## Interface

Server-rendered HTML with HTMX. Charts are SVG drawn in Go; every bar links to
the hosts it counted. All assets ship in the binary, so an estate with no route
to the internet gets a working page.

Filter state lives entirely in the URL. A filtered view pastes into a message
and opens the same thing for the next person.

## What proves it works

```sh
go test ./... -race
```

The tests that matter are the ones that could fail:

- Collector specs run against **captured command output** — AIX, Linux,
  Windows — with hostnames and serials replaced. The layout, spacing and field
  order are the machine's own, not strings written to match the parser.
- The importer must reproduce **the totals a document states about itself**.
  Arithmetic this project did not author.
- Export, restore into an empty database, export again: **byte-identical**.
- Parsers are fuzzed. Garbage must not panic.
- Handler tests assert real HTML, the `Vary: HX-Request` header, and that one
  handler returns both a page and a fragment correctly.
- An external collector is run as a real subprocess and checked for **not**
  being able to read this process's environment.
- A deliberately wrong parse rule is corrected and the value re-derived with
  **no host reachable** in the test at all, which is the claim ADR 0003 makes.
- A failed DNS lookup is checked for not being reported as a missing record.
  The two have different owners.
- Of two sources of equal rank, the one that most recently confirmed its value
  wins — not the one that first said something.

Three bugs were found by writing these, including a normaliser rule with an
empty pattern that matched every value in the estate.

## Status

Early. The schema is stable enough that `export`/`restore` round-trips, and
breaking changes to it will come with a migration. Known gaps are listed in the
threat model rather than left for you to find.

## Licence

MIT. See [LICENSE](LICENSE).
