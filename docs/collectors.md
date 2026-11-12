# Collecting what you care about

KryoState always reads the operating system, its version, the kernel, the
hardware model and the serial number of every host. A **collector** makes it
read something else: the memory size, the version of nginx or OpenSSL,
whether a file exists, the value of `JAVA_HOME`, and so on.

There are two ways to add one:

| | When | Effort |
|---|---|---|
| **Switch one on from the catalog** | What you want is in the list below | A click, or one command |
| **Write your own** | Nothing in the catalog fits | Ten minutes, no programming |

Start with the catalog.

## The catalog

The catalog is a list of ready-made collectors built into KryoState. Each one
runs a read-only command that has already been written for you. The Linux
commands have been run on real Debian and AlmaLinux hosts. The AIX and
Windows commands are checked against recorded output, so test them on one of
your hosts before relying on them. A host that does not have the program is
left blank, not counted as a failure.

| Collector | Records | Systems |
|---|---|---|
| CPU cores | `hw.cpu.cores` | Linux, AIX, Windows |
| Memory total (MB) | `hw.memory.total_mb` | Linux, AIX, Windows |
| Time zone | `os.timezone` | Linux, AIX, Windows |
| DNS servers | `net.dns.servers` | Linux, AIX, Windows |
| SELinux mode | `os.selinux` | Linux |
| Java, Python, OpenSSL, nginx, Apache httpd and Docker versions | `java.version` and so on | varies |
| Installed package version, for a package you name | `pkg.<name>.version` | Linux, AIX, Windows |
| File exists, for a path you give; records `true` or `false` | `file.<name>.exists` | Linux, AIX, Windows |
| File contents, for a path you give | `file.<name>.contents` | Linux, AIX, Windows |
| Setting in a config file, for a file, key and optional section you give | `conf.<file>.<section>.<key>` | Linux, AIX, Windows |
| Environment variable, for a name you give | `env.<name>` | Linux, AIX, Windows |
| Splunk Universal Forwarder version | `splunk.uf.version` | Linux, AIX, Windows |
| Splunk Enterprise version | `splunk.enterprise.version` | Linux, AIX, Windows |
| Splunk home, from the running splunkd | `splunk.home` | Linux, AIX, Windows |

KryoState deliberately has no collectors for CPU or memory **usage**. Those
change by the second, which is the job of a monitoring system.

### Groups

A group switches on several related collectors in one step.

| Group | Switches on |
|---|---|
| Host basics (`group.basics`) | CPU cores, memory total, time zone, DNS servers |
| Language runtimes (`group.runtimes`) | Java, Python and OpenSSL versions |
| Web servers (`group.web`) | nginx and Apache httpd versions |

A group only adds. If CPU cores already runs on every host, switching on Host
basics for one stack leaves it running on every host. If the Web servers
group already runs on one stack, switching it on for a second stack runs it on
both.

### Switching one on in the web interface

1. Open **Collectors**. It lists every collector: the built-in ones, the
   catalog, your own and external programs. Type in the search box, or narrow
   the list by category, source (built-in, catalog, your own, external) or
   status (on or off).
2. Select the collector to open its page. Under **Where it runs**, open
   **Switch on** (or **Add another** for the ones that take a path or a name).
3. Under **Run on**, keep **All hosts**, or choose **Only hosts in these
   stacks**. For example, run the nginx version only on your web
   stacks.
4. Select **Switch on**.
5. Back under **Where it runs**, choose a host and select **Test** to see what
   it reads. Nothing is saved by a test. **Turn off** is next to it.

The groups are the buttons above the list: open one, choose where it runs and
switch it on.

Two shortcuts do the same thing from elsewhere. On the **Stacks** page, each
stack's card has **Also collect on**, which switches a group or a collector on
for that stack. On a host's page, **Test a collector** runs any switched-on
collector on that host.

It is read the next time it is due: once a day for most collectors, every 6
hours for file and config checks, while `kryostate serve` runs. To read it
straight away, open **Runs** and select **Collect now**. The collector's page
has a **Schedule** card to change how often it runs. The value appears on each
host's page, and you can show it as a column on the **Inventory** page with
**Columns**.

### Switching one on from the command line

```sh
kryostate collectors -catalog                                  # see the list
kryostate collectors -try memory.total -host web01             # try it, saves nothing
kryostate collectors -enable memory.total                      # switch it on everywhere
kryostate collectors -enable package.version -param openssl
kryostate collectors -enable file.exists -param /etc/resolv.conf
kryostate collectors -enable env.var -param JAVA_HOME
kryostate collectors -enable group.web -stacks web-prod
kryostate collectors -enable config.value \
    -param /etc/app/app.conf -section database -key host
kryostate collectors                                           # what is switched on
kryostate collectors -disable 3                                # by its number in that list
```

For a collector that takes a value, KryoState picks the name it is recorded
under, such as `file.resolv_conf.exists`. Choose your own with `-attr`, or in
the **Record as** box on the web page.

### A setting in a config file

**Setting in a config file** reads one `key = value` line from an `.ini`,
`.conf` or `.properties` file. You give three things:

| Field | Example | Meaning |
|---|---|---|
| File | `/etc/app/app.conf` | The full path |
| Key | `host` | The name to the left of the `=` |
| Section | `database` | Optional. Only read the key inside `[database]` |

Without a section, the first matching key anywhere in the file is used.
Spaces around the `=` do not matter. Lines starting with `#` or `;` are
comments and are never matched. A host without the file, or a file without
the key, is left blank.

Useful ones are `max_connections` in PostgreSQL's `postgresql.conf`,
`bind-address` in MySQL's `my.cnf` under `[mysqld]`, and `memory_limit` in
PHP's `php.ini` under `[PHP]`. Files written as `Key value` without an `=`,
such as `sshd_config`, are not read this way.

### File contents, config settings and environment variables

**File contents**, **Setting in a config file** and **Environment variable**
show what is inside something on the host, which may be a password or a key.
An app's config file, for example, often holds a database password. They can always be
switched on from the command line. The web page offers them only if KryoState
was started with:

```sh
kryostate serve -allow-content-collectors
```

Use that only when people must log in before they reach the page, for
example through a proxy that asks for a login.

A path or a name you give is checked before it is used: it may only contain
letters, digits, spaces and a few punctuation marks that are common in real
paths. KryoState puts it inside quotes in the command, so it can never be
turned into a different command.

## Writing your own

When nothing in the catalog fits, you can write a collector of your own. It is
three things written down together:

1. **A command** to run on the host, such as `nginx -v`.
2. **A rule** that says which part of the output to keep.
3. **A name** for the value, such as `nginx.version`.

For example:

| | |
|---|---|
| The command | `nginx -v 2>&1` |
| The host prints | `nginx version: nginx/1.24.0` |
| The rule | keep the numbers after `nginx/` |
| KryoState records | `nginx.version = 1.24.0` |

nginx is also in the catalog; it is used as the example here because its
output has a typical shape. The six steps below take you from nothing to a
working collector. You do not need to write a program.

### Start from a worked example

Every collector's page in the web interface has **View definition**: that
collector written out exactly as you would write it in your own
`collectors.json`, with a **Copy** button. The catalog entries and the
built-in collectors are written in the same format as yours, so the nearest
one to what you want is a good starting point. Entries that take a path or a
name are shown with an example value, such as `/etc/resolv.conf`. The same
is printed on the command line:

```
kryostate collectors -show cpu.cores
kryostate collectors -show file.exists -param /etc/hosts
kryostate collectors -show builtin.os.version
```

Copy the specs into your file and give each one an `id` of its own: ids must
be unique, and the built-in ones are taken.

### Before you start

You need one host that KryoState already collects from. Open its page in the
web interface: the **Reachability** card should show `collector_run` as `ok`.
If it does not, fix that first; the [user guide](user-guide.md#4-tutorial-add-your-first-host)
shows how.

### Step 1: find a command that prints the value

Log in to the host **as the same user KryoState uses** and try commands until
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
error stream instead of normal output. KryoState only reads normal output, so
add `2>&1` to the end of the command.

On Windows, KryoState runs the command in PowerShell.

### Step 2: create your collectors file

Run this once:

```sh
kryostate collectors -init
```

It creates a file called `collectors.json` next to the KryoState database, and
prints where it is. Every KryoState command, and the web interface, reads your
collectors from that file. You never need to name it on the command line.

The file already contains one working example:

```json
{
  "version": 1,
  "specs": [
    {
      "id": "example.nodename",
      "attr": "host.nodename",
      "os": ["linux", "aix"],
      "comment": "An example to copy. It runs 'uname -n' and keeps the first line: the name the host calls itself.",
      "command": "uname -n",
      "parse": { "kind": "first_line" },
      "type": "text"
    }
  ]
}
```

### Step 3: describe your collector

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
| `type` | What kind of value it is | Use `"text"`. For a yes-or-no answer use `"bool"` (see "Yes-or-no answers" below) |
| `stacks` | Optional. Only run on hosts in these stacks | A list of stack names, such as `["web-prod"]`. Leave it out to run on every host |

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

`"optional": true` tells KryoState that some hosts do not have nginx, and that
this is fine. Without it, a host without nginx is reported as a partly failed
collection.

### Step 4: choose how to read the output

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
| several lines, all wanted (a small file) | `{ "kind": "all" }` | every line, up to 8 KB |

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
- **all** keeps the whole output, without blank lines at the start and end.
  It is cut at 8 KB, so use it for small files only.

**A common mistake with regex:** inside a JSON file, a backslash must be
written twice. The pattern `\d+` must be written `"\\d+"`. To avoid this,
write `[0-9]` instead of `\d`, and `[ ]` instead of `\s`.

### Yes-or-no answers

When the question is yes or no, such as "is this file there?" or "is this
service enabled?", make the command print exactly `true` or `false`, and set
`"type": "bool"`. This is the good practice for every yes-or-no collector, and
it is what the catalog's **File exists** does:

```json
{
  "id": "linux.myapp.installed",
  "attr": "myapp.installed",
  "os": ["linux"],
  "command": "if [ -x /opt/myapp/bin/myapp ]; then echo true; else echo false; fi",
  "parse": { "kind": "first_line" },
  "type": "bool"
}
```

Do not print `yes`/`no`, `1`/`0` or `installed`/`missing`. With `true` and
`false`, every host answers in the same words, the charts show two clear
groups, and an inventory query can test it simply: `myapp.installed` finds
the hosts where it is true, and `!myapp.installed` the rest.

### Step 5: test it

Test your collector against one host. **Nothing is saved**, so you can try as
many times as you like:

```sh
kryostate collectors -try linux.nginx.version -host web01
```

KryoState connects, runs the command, and shows you what happened:

```text
collector  linux.nginx.version
records    nginx.version
command    nginx -v 2>&1
host       web01 (svc-ro@web01.example.com, linux)

--- what the command printed ---
nginx version: nginx/1.24.0
--------------------------------

RESULT     nginx.version = "1.24.0"
```

If the result is not what you expected, the message says why and what to try.

You can also test the parse rule with no host at all. Save the command's
output from step 1 in a file, then:

```sh
kryostate collectors -try linux.nginx.version -sample nginx-output.txt
```

This is the quickest way to get a regex right: edit the pattern, run the
command again, repeat.

### Step 6: collect

```sh
kryostate refresh
```

Your value now appears:

- on each host's page, in the **Facts** table;
- in the inventory, if you add it as a column (**Columns** on the Inventory
  page, or `kryostate hosts -cols nginx.version`);
- on the **Charts** page, once some hosts have it.

If `kryostate serve` was already running when you created or changed the file,
restart it so that the web interface uses the new version. The
**Collectors** page in the web interface lists every collector that will run;
choose **your own** under the source filter to see yours. Each one's page
shows its command, its rule, a **Test** button and its **Schedule**. Your own
collectors run once a day by default while `kryostate serve` runs; change
that on the collector's page, or with
`kryostate collectors -schedule spec.linux.nginx.version -every 6h`. If the
file has a mistake, the page says what and where, rather than the server
refusing to start.

## When it does not work

| What you see | What it means | What to do |
|---|---|---|
| `line 7, column 5: invalid character` | The file is not valid JSON | Look just before that position. Usually a comma is missing between two collectors, or there is one too many after the last one |
| `unknown field "optinal"` | A word is misspelled | Correct the spelling. KryoState refuses unknown words so that a typo is never ignored |
| `duplicate collector id` or `is already defined` | Two collectors have the same `id`, or yours repeats a built-in one | Give each one a different id |
| `the command failed: ... exited 127` | The program is not installed, or not on the PATH | Use the full path, such as `/usr/sbin/nginx -v 2>&1`, or mark the collector optional |
| `the parse rule (regex) found nothing` | The command worked, but the rule did not match | Compare the rule with the printed output. Test changes with `-sample` |
| The output is empty | The program printed to the error stream | Add `2>&1` to the end of the command |
| `NOTE: the host says it is ...` | The host's system is not in `os` | Add it to the `os` list |
| The value never appears after `refresh` | The file was not found, the host is not in `os`, or the collector is limited to `stacks` the host is not in | Run `kryostate collectors` and check your collector is listed |
| `"C:/Program Files/Git/etc/..." is not allowed` | Git Bash on Windows rewrote a path that starts with `/` | Run `export MSYS_NO_PATHCONV=1` first, or use PowerShell or the web page |

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
      "id": "windows.nodename",
      "attr": "host.nodename",
      "os": ["windows"],
      "command": "$env:COMPUTERNAME",
      "parse": { "kind": "first_line" },
      "type": "text"
    }
  ]
}
```

Two collectors can record the same `attr` for different systems, as long as
their `id` is different. For example, the starter collector records
`host.nodename` on Linux and AIX, and the second collector above records it
on Windows.

## Going further

### Comparing versions

Give a version an attribute name that ends in `.version`, such as
`nginx.version`. KryoState then groups it by its first two numbers, so the
charts put `1.24.0` and `1.24.3` together under `1.24`. The full value stays
in the **Raw** column on the host page. This works for versions written as
two, three or four numbers separated by dots, such as `1.24` or `9.4.1.2`.

If your program prints versions in another shape, such as `2024 R2`, add a
**normaliser rule** that says how to read them. Once some hosts' values of an
attribute are read by a rule, a value the rules cannot read is shown as "not
understood", so you can see what the rule is missing. To add a rule, create
`rules.json` next to `collectors.json`. This one reads `2024 R2` as version
2024.2:

```json
{
  "version": 1,
  "rules": [
    {
      "id": "myapp-release",
      "attr": "myapp.version",
      "pattern": "^(?P<major>[0-9]{4}) R(?P<minor>[0-9]+)$",
      "major": "${major}",
      "minor": "${minor}",
      "value": "${major}.${minor}"
    }
  ]
}
```

`pattern` is a Go regular expression with named parts; `value` is what is
shown and charted. Your rules are tried before the built-in ones. After
changing a rule, `kryostate renormalize` applies it to the values already
collected.

### Several copies of one program on a host

If a host runs two instances of something, write one collector for each and
give each a different `scope`, such as `"scope": "instance-a"`. The values
are recorded side by side instead of replacing each other.

### Fixing a rule after collecting

KryoState keeps the last output of every command. After you correct a `parse`
rule, apply it to what was already collected, without connecting to any host:

```sh
kryostate reparse -spec linux.nginx.version
```

### External programs

When the value is not on the host at all, for example it is in a hypervisor
or a cloud API, write a small program instead. KryoState runs it on the machine
that runs KryoState, once for each host, and reads JSON from it.

The program receives the host's name in `KRYOSTATE_TARGET` and its address in
`KRYOSTATE_ADDRESS`. It must print:

```json
{
  "schema": "kryostate.facts/v1",
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
      "path": "/opt/kryostate/collectors/vmware-facts",
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
| `type` | no | `text` (default), `version`, `path`, `number`, `bool` (the command prints `true` or `false`) or `list` |
| `optional` | no | `true` when some hosts legitimately lack the value |
| `stacks` | no | Only run on hosts in these stacks |
| `scope` | no | Separates several instances on one host |
| `comment` | no | A note for people. KryoState ignores it |

## Why your own collectors are not written in the web interface

A collector you write is a command that KryoState runs on every host. The web
interface has no login of its own, so if it could take a command, anyone who
can open the page could run commands across your estate. Your own collectors
therefore live in a file that only people with access to the KryoState machine
can change.

The catalog is different: its commands are built into KryoState, and the page
can only switch them on or off and choose where they run. A path or a name
you give a catalog collector is checked and quoted, so it cannot become a
different command.
