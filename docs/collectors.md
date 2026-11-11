# Writing your own collector

hearsay already reads the operating system, kernel, hardware model and serial
number of every host. A **collector** lets it read anything else you care
about, such as the version of nginx, Java or your own application.

This page takes you from nothing to a working collector in six steps. You do
not need to know how hearsay works inside, and you do not need to write a
program.

## What a collector is

A collector is three things written down together:

1. **A command** to run on the host, such as `nginx -v`.
2. **A rule** that says which part of the output to keep.
3. **A name** for the value, such as `nginx.version`.

For example:

| | |
|---|---|
| The command | `nginx -v 2>&1` |
| The host prints | `nginx version: nginx/1.24.0` |
| The rule | keep the numbers after `nginx/` |
| hearsay records | `nginx.version = 1.24.0` |

hearsay runs the command on every host each time it collects, records the
value, and compares it with what your documents say.

## Before you start

You need one host that hearsay already collects from. Open its page in the
web interface: the **Reachability** card should show `collector_run` as `ok`.
If it does not, fix that first; the [user guide](user-guide.md#4-tutorial-add-your-first-host)
shows how.

## Step 1: find a command that prints the value

Log in to the host **as the same user hearsay uses** and try commands until
one prints what you want. Good commands:

- **only read**. Never use a command that changes anything.
- **finish in a few seconds** and do not ask questions.
- **print the value on a line of their own**, or close to it.

Examples:

| To read | Linux and AIX command | Windows (PowerShell) command |
|---|---|---|
| nginx version | `nginx -v 2>&1` | |
| Java version | `java -version 2>&1` | |
| Time zone | `date +%Z` | `(Get-TimeZone).Id` |
| A value in a file | `cat /etc/myapp/version` | `Get-Content C:\myapp\version.txt` |
| Installed package | `rpm -q --qf '%{VERSION}' openssl` | |

**Tip:** some programs, including `nginx -v` and `java -version`, print to the
error stream instead of normal output. hearsay only reads normal output, so
add `2>&1` to the end of the command.

On Windows, hearsay runs the command in PowerShell.

## Step 2: create your collectors file

Run this once:

```sh
hearsay collectors -init
```

It creates a file called `collectors.json` next to the hearsay database, and
prints where it is. Every hearsay command, and the web interface, reads your
collectors from that file. You never need to name it on the command line.

The file already contains one working example:

```json
{
  "version": 1,
  "specs": [
    {
      "id": "example.timezone",
      "attr": "host.timezone",
      "os": ["linux", "aix"],
      "comment": "An example to copy. It runs 'date +%Z' and keeps the first line, such as UTC.",
      "command": "date +%Z",
      "parse": { "kind": "first_line" },
      "type": "text"
    }
  ]
}
```

## Step 3: describe your collector

Open the file in any text editor and change the example into your collector.
Each line means:

| Line | What it is | What to write |
|---|---|---|
| `id` | A unique name for this collector | Anything unique. A good pattern is system, then what it reads: `linux.nginx.version` |
| `attr` | The name the value is recorded under | Lowercase words joined by dots: `nginx.version`. This is what you will see on host pages |
| `os` | Which systems to run on | Any of `"linux"`, `"aix"`, `"windows"`, in a list |
| `comment` | A note for the next person | Optional. Say why the command is written the way it is |
| `command` | The command from step 1 | Copy it exactly |
| `parse` | Which part of the output to keep | See step 4 |
| `type` | What kind of value it is | Use `"text"`. See "Comparing versions" below for the other choices |

Here is the nginx collector, complete:

```json
{
  "version": 1,
  "specs": [
    {
      "id": "linux.nginx.version",
      "attr": "nginx.version",
      "os": ["linux"],
      "comment": "nginx -v writes to stderr, so 2>&1 is needed.",
      "command": "nginx -v 2>&1",
      "parse": { "kind": "regex", "pattern": "nginx/([0-9.]+)" },
      "type": "text",
      "optional": true
    }
  ]
}
```

`"optional": true` tells hearsay that some hosts do not have nginx, and that
this is fine. Without it, a host without nginx is reported as a partly failed
collection.

## Step 4: choose how to read the output

The `parse` line picks the value out of what the command printed. Find the
row that looks like your output:

| If the output looks like | Write this | And you get |
|---|---|---|
| `UTC` (the value is the whole first line) | `{ "kind": "first_line" }` | `UTC` |
| `nginx version: nginx/1.24.0` (the value is part of a line) | `{ "kind": "regex", "pattern": "nginx/([0-9.]+)" }` | `1.24.0` |
| `VERSION_ID="8.6"` (lines of `NAME=value`) | `{ "kind": "kv", "key": "VERSION_ID" }` | `8.6` |
| `Version : 10.0.17763` (lines of `Name : value`) | `{ "kind": "colon_kv", "key": "Version" }` | `10.0.17763` |
| `bos.rte  7.2.5.0  C  F  Base` (columns) | `{ "kind": "field", "marker": "bos.rte", "field": 2 }` | `7.2.5.0` |
| `{"app": {"version": "4.2"}}` (JSON) | `{ "kind": "json_path", "path": "app.version" }` | `4.2` |

More detail on each:

- **first_line** keeps the first line that is not blank. It is the simplest
  choice. Use it whenever you can make the command print only the value.
- **regex** keeps the part of the output inside the round brackets. The
  pattern is a Go regular expression. `[0-9.]+` means "digits and dots", which
  matches most version numbers. If the pattern has several bracketed parts,
  name the one to keep `(?P<v>...)`.
- **kv** reads `NAME=value` lines and removes quotes around the value. The
  key must match exactly, including upper and lower case.
- **colon_kv** reads `Name : value` lines, as PowerShell's `Format-List`
  prints them. Upper and lower case do not matter.
- **field** finds the first line that contains the marker text, splits it on
  spaces, and keeps column number `field`. The first column is 1.
- **json_path** reads JSON and follows the names joined by dots.

**A common mistake with regex:** inside a JSON file, a backslash must be
written twice. The pattern `\d+` must be written `"\\d+"`. To avoid this,
write `[0-9]` instead of `\d`, and `[ ]` instead of `\s`.

## Step 5: test it

Test your collector against one host. **Nothing is saved**, so you can try as
many times as you like:

```sh
hearsay collectors -try linux.nginx.version -host web01
```

hearsay connects, runs the command, and shows you what happened:

```text
collector  linux.nginx.version
records    nginx.version
command    nginx -v 2>&1
host       web01 (svc-ro@web01.example.com)

--- what the command printed ---
nginx version: nginx/1.24.0
--------------------------------

RESULT     nginx.version = "1.24.0"
```

If the result is not what you expected, the message says why and what to try.

You can also test the parse rule with no host at all. Save the command's
output from step 1 in a file, then:

```sh
hearsay collectors -try linux.nginx.version -sample nginx-output.txt
```

This is the quickest way to get a regex right: edit the pattern, run the
command again, repeat.

## Step 6: collect

```sh
hearsay refresh
```

Your value now appears:

- on each host's page, in the **Facts** table;
- in the inventory, if you add it as a column (**Columns** on the Inventory
  page, or `hearsay hosts -cols nginx.version`);
- on the **Charts** page, once some hosts have it.

If `hearsay serve` was already running when you created or changed the file,
restart it so that the web interface uses the new version. The
**Collectors** page in the web interface lists every collector that will run,
yours first.

## When it does not work

| What you see | What it means | What to do |
|---|---|---|
| `line 7, column 5: invalid character` | The file is not valid JSON | Look just before that position. Usually a comma is missing between two collectors, or there is one too many after the last one |
| `unknown field "optinal"` | A word is misspelled | Correct the spelling. hearsay refuses unknown words so that a typo is never ignored |
| `duplicate collector id` or `is already defined` | Two collectors have the same `id`, or yours repeats a built-in one | Give each one a different id |
| `the command failed: ... exited 127` | The program is not installed, or not on the PATH | Use the full path, such as `/usr/sbin/nginx -v 2>&1`, or mark the collector optional |
| `the parse rule (regex) found nothing` | The command worked, but the rule did not match | Compare the rule with the printed output. Test changes with `-sample` |
| The output is empty | The program printed to the error stream | Add `2>&1` to the end of the command |
| `NOTE: the host says it is ...` | The host's system is not in `os` | Add it to the `os` list |
| The value never appears after `refresh` | The file was not found, or the host is not in `os` | Run `hearsay collectors` and check your collector is listed |

## Adding more collectors to the file

Put each collector inside the `specs` list, between `{` and `}`, with a comma
**between** them and **no** comma after the last one:

```json
{
  "version": 1,
  "specs": [
    {
      "id": "linux.nginx.version",
      "attr": "nginx.version",
      "os": ["linux"],
      "command": "nginx -v 2>&1",
      "parse": { "kind": "regex", "pattern": "nginx/([0-9.]+)" },
      "type": "text",
      "optional": true
    },
    {
      "id": "windows.timezone",
      "attr": "host.timezone",
      "os": ["windows"],
      "command": "(Get-TimeZone).Id",
      "parse": { "kind": "first_line" },
      "type": "text"
    }
  ]
}
```

Two collectors can record the same `attr` for different systems, as long as
their `id` is different. For example, the starter collector records
`host.timezone` on Linux and AIX, and the second collector above records it
on Windows.

## Going further

### Comparing versions

With `"type": "text"`, hearsay compares values exactly as written. That is
right for most things.

With `"type": "version"`, hearsay expects a **normaliser rule** that turns the
text into a version it can compare and chart by major and minor number. A
version with no matching rule is reported as "not understood". To add a rule,
create `rules.json` next to `collectors.json`:

```json
{
  "version": 1,
  "rules": [
    {
      "id": "nginx-version",
      "attr": "nginx.version",
      "pattern": "^(?P<major>[0-9]+)[.](?P<minor>[0-9]+)[.](?P<patch>[0-9]+)$",
      "family": "nginx",
      "major": "${major}",
      "minor": "${minor}",
      "patch": "${patch}",
      "value": "nginx ${major}.${minor}.${patch}"
    }
  ]
}
```

After changing a rule, `hearsay renormalize` applies it to the values already
collected.

### Several copies of one program on a host

If a host runs two instances of something, write one collector for each and
give each a different `scope`, such as `"scope": "instance-a"`. The values
are recorded side by side instead of replacing each other.

### Fixing a rule after collecting

hearsay keeps the last output of every command. After you correct a `parse`
rule, apply it to what was already collected, without connecting to any host:

```sh
hearsay reparse -spec linux.nginx.version
```

### External programs

When the value is not on the host at all, for example it is in a hypervisor
or a cloud API, write a small program instead. hearsay runs it on the machine
that runs hearsay, once for each host, and reads JSON from it.

The program receives the host's name in `HEARSAY_TARGET` and its address in
`HEARSAY_ADDRESS`. It must print:

```json
{
  "schema": "hearsay.facts/v1",
  "collector": "vmware",
  "status": "ok",
  "facts": [
    {"key": "vm.cluster", "value": "prod-cluster-2", "type": "text"}
  ]
}
```

Each `key` needs at least one dot. List the program in `externals.json`, next
to `collectors.json`:

```json
{
  "version": 1,
  "externals": [
    {
      "name": "vmware",
      "path": "/opt/hearsay/collectors/vmware-facts",
      "timeout": "30s",
      "env": ["VCENTER_URL=https://vcenter.example.com"]
    }
  ]
}
```

The program gets no other environment variables and no credentials, except
the ones you list in `env`.

## Every field, for reference

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | Unique name. Must not repeat a built-in id |
| `attr` | yes | Name the value is recorded under |
| `os` | yes | List of `linux`, `aix`, `windows` |
| `command` | yes | Command to run. Read-only |
| `parse` | yes | How to read the output: `kind`, plus `pattern`, `key`, `marker` and `field`, or `path` |
| `type` | no | `text` (default), `version`, `path`, `number`, `flag` or `list` |
| `optional` | no | `true` when some hosts legitimately lack the value |
| `scope` | no | Separates several instances on one host |
| `comment` | no | A note for people. hearsay ignores it |

## Why collectors are not edited in the web interface

A collector is a command that hearsay runs on every host. The web interface
has no login of its own, so if it could change a collector, anyone who can
open the page could run commands across your estate. Collectors therefore
live in a file that only people with access to the hearsay machine can
change.
