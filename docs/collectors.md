# Collecting what you care about

This guide is for operators who want KryoState to read more values from
their hosts. It tells you how to switch on a collector from the catalog and
how to write your own collector. It also gives the reference for each
collector field.

A **collector** reads one value from each host, such as the memory size, the
version of nginx, whether a file exists, or the value of `JAVA_HOME`. There
are five sources of collectors:

| Source | What it is | How you get it |
|---|---|---|
| built-in | Commands in the KryoState program. They read the operating system, its version, the kernel, the hardware model, the serial number and the machine ID, where the system has them. | They always run. You cannot change them. |
| catalog | Ready-made collectors in the KryoState program | Switch one on in the web interface or with `kryostate collectors enable` |
| added | Your own collectors, kept in the database | `kryostate collectors add <file.json>` |
| your own | Your own collectors in the collectors file, `collectors.json` | Edit the file |
| external | Programs that run on the KryoState server, not on the host | List them in `externals.json` |

Use the catalog first. Write your own collector only if no catalog entry
gives the value.

## The catalog

Each catalog command only reads. Each catalog collector is optional: if a
host does not have the program, the value stays blank and the run does not
fail.

**CAUTION:** The AIX and Windows catalog commands are tested only against
recorded output. Test each one on one of your hosts before you use its
values. The Linux commands are tested on Debian and AlmaLinux hosts.

| Catalog name | Title | Records | Systems |
|---|---|---|---|
| `cpu.cores` | CPU cores | `hw.cpu.cores` | Linux, AIX, Windows |
| `memory.total` | Memory total (MB) | `hw.memory.total_mb` | Linux, AIX, Windows |
| `timezone` | Time zone | `os.timezone` | Linux, AIX, Windows |
| `dns.servers` | DNS servers | `net.dns.servers` | Linux, AIX, Windows |
| `selinux` | SELinux mode | `os.selinux` | Linux |
| `java.version` | Java version | `java.version` | Linux, AIX, Windows |
| `python.version` | Python version | `python.version` | Linux, AIX, Windows |
| `openssl.version` | OpenSSL version | `openssl.version` | Linux, AIX |
| `docker.version` | Docker version | `docker.version` | Linux |
| `nginx.version` | nginx version | `nginx.version` | Linux |
| `httpd.version` | Apache httpd version | `httpd.version` | Linux |
| `package.version` | Installed package version, for a package that you name | `pkg.<name>.version` | Linux, AIX, Windows |
| `file.exists` | File exists, for a path that you give. Records `true` or `false`. | `file.<name>.exists` | Linux, AIX, Windows |
| `file.contents` | File contents, for a path that you give. Up to 8 KB. | `file.<name>.contents` | Linux, AIX, Windows |
| `config.value` | Setting in a config file, for a file, a key and an optional section that you give | `conf.<file>.<section>.<key>` | Linux, AIX, Windows |
| `env.var` | Environment variable, for a name that you give | `env.<name>` | Linux, AIX, Windows |

KryoState makes `<name>` from the last part of the path or from the name. It
uses lowercase letters, and it changes each group of other characters to `_`.
For example, `/etc/resolv.conf` gives `file.resolv_conf.exists`.

KryoState has no collectors for CPU usage or memory usage. These values
change each second. Use a monitoring system for them.

### Groups

A group switches on several catalog collectors in one step.

| Group | Switches on |
|---|---|
| Host basics (`group.basics`) | CPU cores, memory total, time zone, DNS servers |
| Language runtimes (`group.runtimes`) | Java, Python and OpenSSL versions |
| Web servers (`group.web`) | nginx and Apache httpd versions |

A group only adds hosts. It does not remove hosts. For example, if CPU cores
runs on all hosts, Host basics for one stack does not change that.

### Switch on a collector in the web interface

1. Open **Collectors**.
2. Find the collector. Type in the search box, or filter by category, source
   or status.
3. Select the collector to open its page.
4. Under **Where it runs**, select **Switch on**. For a collector that takes a
   path or a name, select **Add another**.
5. Under **Runs on**, write a query for the hosts. Leave it blank for all
   hosts.
6. Select **Switch on**.

The preview under the **Runs on** box shows how many hosts match, and the
names of the first hosts. To switch on a group, select its button above the
list, then do steps 5 and 6.

To test the collector on one host:

1. Under **Test on a host**, type part of a host name.
2. Select the host. The list shows only hosts that the collector runs on and
   that have a target.
3. Select **Test**. KryoState shows the value and does not save it.

To switch the collector off, select **Turn off**.

### Choose where a collector runs

Where a collector runs is a query in the language of the **Inventory** search
box (see the [query guide](queries.md)). A blank query means all hosts.

| Query | Runs on |
|---|---|
| `stack == "web"` | The members of the web stack |
| `stack in ("web", "edge")` | The members of the two stacks |
| `name in ("web01", "web02", "db01")` | These three hosts only |
| `os.family == "Linux" && stack == "web"` | The Linux members of the web stack |
| `stack == "web" \|\| name == "db01"` | The web stack, and db01 |
| `!name in ("web03")` | All hosts except web03 |

The editor helps you write the query:

- The **Stack** and **System** buttons add a condition, such as
  `stack == "web"`, with `||`. There are buttons for the first 12 stacks
  only.
- **Add a host by name** finds hosts as you type, and adds the host to the
  `name in (...)` list.
- The preview counts the hosts that match. It also counts the hosts that
  KryoState skips, because the collector has no command for their system.
- If the query does not parse, the editor shows the column of the problem.

Each host runs the command for its own system. Thus a query does not have to
separate Linux and Windows hosts. The collector page shows each command.

Each switched-on collector shows where it runs, such as **Hosts in web** or
**Only web01, web02**, with **matches N hosts**. Select **matches N hosts**
to see these hosts in the **Inventory**. Select **Change where it runs** to
edit the query.

**CAUTION:** If a stored query does not parse, the collector runs on no
host. It does not run on all hosts. The collector page tells you why.

On the page of a host, **Collectors on this host** shows the collectors that
run there, why, and on what schedule. It also shows the switched-on
collectors that skip the host, and why.

### When a catalog collector runs

While `kryostate serve` runs, a collector runs on its schedule. The default
schedule is one time each day. For File exists, File contents, Setting in a
config file and Environment variable, the default is every 6 hours. To
change the schedule, use the **Schedule** card on the collector page.

To collect now:

1. Open **Runs**.
2. Select **Collect now**.

The value shows on the page of each host. To show it as a column on the
**Inventory** page, use **Columns**.

### Switch on a collector from the command line

```sh
kryostate collectors catalog                                   # show the catalog
kryostate collectors try memory.total --host web01             # test it, save nothing
kryostate collectors enable memory.total                       # switch it on for all hosts
kryostate collectors enable package.version --param openssl
kryostate collectors enable file.exists --param /etc/resolv.conf
kryostate collectors enable env.var --param JAVA_HOME
kryostate collectors enable group.web --stacks web-prod
kryostate collectors enable nginx.version --where 'name in ("web01", "web02")'
kryostate collectors enable java.version --where 'stack == "app" && os.family == "Linux"'
kryostate collectors enable config.value \
    --param /etc/app/app.conf --section database --key host
kryostate collectors list                                      # show what is switched on
kryostate collectors disable 3                                 # by its number in the list
```

| Flag of `collectors enable` | Use |
|---|---|
| `--where '<query>'` | The hosts it runs on. Blank means all hosts. If the query does not parse, the command shows a `^` under the problem and saves nothing. |
| `--stacks a,b` | The same as `--where 'stack in ("a", "b")'`. If you give `--stacks` and `--where`, a host must match the two. |
| `--param <value>` | The path, variable name or package name that a collector needs |
| `--section`, `--key` | For `config.value`: the section and the key |
| `--attr <name>` | The attribute name to record the value as. In the web interface, this is the **Record as** box. |

Put the query in single quotes, so that the shell does not change the double
quotes. `collectors enable` replaces the hosts where a collector runs. For a
group, it only adds hosts. `collectors disable` takes the number from
`collectors list`, or the catalog name with `--param`.

### A setting in a config file

**Setting in a config file** (`config.value`) reads one `key = value` line
from an `.ini`, `.conf` or `.properties` file. Give three values:

| Field | Example | Meaning |
|---|---|---|
| File | `/etc/app/app.conf` | The full path |
| Key | `host` | The name to the left of `=`. Upper and lower case must match. |
| Section | `database` | Optional. Read the key only inside `[database]`. |

- Without a section, KryoState uses the first line with the key in the file.
- Spaces around `=` are not important.
- Lines that start with `#` or `;` are comments. KryoState does not read
  them.
- If the host has no file, or the file has no key, the value stays blank.

Examples are `max_connections` in `postgresql.conf`, `bind-address` under
`[mysqld]` in `my.cnf`, and `memory_limit` under `[PHP]` in `php.ini`.
KryoState cannot read files in the `Key value` format without `=`, such as
`sshd_config`.

### Collectors that read content

**WARNING:** File contents, Setting in a config file and Environment variable
show content from the host. This content can be a password or a key. For
example, an application config file often contains a database password. Do
not let unauthenticated users see these values.

You can always switch these collectors on from the command line. The web
interface offers them only if you start KryoState with this flag:

```sh
kryostate serve --allow-content-collectors
```

Use this flag only if users must log in before they get to the web interface,
for example through a proxy that asks for a login.

KryoState checks each value that you give before it uses it. Then it puts the
value in single quotes in the command, so that the value cannot become a
different command.

| Value | Permitted |
|---|---|
| Path | A full path that starts with `/` or a drive letter, 255 characters maximum. Letters, digits, spaces and `. _ - + , @ = ~ : ( ) / \`. No `..`. |
| Variable name | Letters, digits and `_`, 64 characters maximum. It must not start with a digit. |
| Package name | Letters, digits, spaces and `. _ + -`, 100 characters maximum. It must start with a letter or a digit. |
| Config key | Letters, digits and `. _ -`, 64 characters maximum |
| Config section | Letters, digits, spaces and `. _ - : / =`, 100 characters maximum |

## Writing your own

If no catalog entry gives the value, write a collector. You do not have to
write a program. A collector has three parts:

| Part | Example |
|---|---|
| A command to run on the host | `nginx -v 2>&1`, which prints `nginx version: nginx/1.24.0` |
| A parse rule that tells which part of the output to keep | Keep the digits and dots after `nginx/` |
| An attribute name for the value | `nginx.version`, which records `1.24.0` |

nginx is also in the catalog. This guide uses it as the example because its
output has a typical format.

You can keep your collectors in the database or in the collectors file. The
JSON format of a collector is the same for the two.

| | In the database | In the collectors file |
|---|---|---|
| Add or change | `kryostate collectors add <file.json>` | Edit `collectors.json` next to the database, or the file that you give with `--collectors` |
| A running `kryostate serve` | Uses the change at the next run and page view. No restart is necessary. | You must restart it. |
| Remove | `kryostate collectors rm <id>` | Remove it from the file |
| Web interface label | **added** | **your own** |

The web interface shows these collectors, but it cannot create, change or
remove them. The web interface has no login of its own. If it accepted
commands, all users who can open the page could run commands on all hosts.

If two collectors have the same `id`, the order of precedence is: built-in,
then the collectors file, then added. The collectors file cannot use the
`id` of a built-in collector. If an added collector has the same `id` as a
collector in the file, KryoState uses the file and skips the added collector.
The server log and `kryostate collectors list` tell you about this.

### Add a collector to the database

1. Run `kryostate collectors add --example > mine.json` to get a working
   example.
2. Edit `mine.json`. Steps 1, 3, 4 and 5 below tell you how.
3. Run `kryostate collectors add mine.json --dry-run` to check the file. This
   saves nothing.
4. Run `kryostate collectors add mine.json`.

```json
{
  "id": "example.kernel",
  "attr": "custom.kernel",
  "os": ["linux"],
  "comment": "An example to copy. It runs 'uname -r' and keeps the first line: the kernel release.",
  "command": "uname -r",
  "parse": { "kind": "first_line" },
  "type": "version"
}
```

The file can contain one collector, a list (`[ {...}, {...} ]`), or a full
collectors file (`{"version": 1, "specs": [...]}`). To read the file from
standard input, give `-` as the file name. KryoState checks all collectors in
the file before it saves one. If one collector has an error, KryoState saves
nothing.

- If you add an `id` that you added before, KryoState replaces it and shows
  `updated`.
- `kryostate collectors rm <id>` removes an added collector. KryoState keeps
  the values that it collected. You cannot remove other collectors this way.
- `kryostate collectors list` shows added collectors under *Added with
  kryostate collectors add*, with who added them and when.
- The audit log records each add, replacement and removal.

### Start from a worked example

Each collector page in the web interface has **View definition**, with a
**Copy** button. It shows the collector in the JSON format of
`collectors.json`. For a collector that takes a path or a name, it uses an
example value. `kryostate collectors show` prints the same, and tells you
where the collector comes from:

```sh
kryostate collectors show cpu.cores
kryostate collectors show file.exists --param /etc/hosts
kryostate collectors show builtin.os.version
```

Copy the definition into your file and give it a new, unique `id`.

### Before you start

You need one host that KryoState collects from now. On the page of the host,
the **Reachability** card must show `collector_run` as `ok`. If it does not,
correct this first. The [user guide](user-guide.md#4-tutorial-add-your-first-host)
tells you how.

### Step 1: find a command that prints the value

Log in to the host as the user that KryoState uses. Try commands until one
prints the value. The command must:

- only read. Do not use a command that changes something.
- stop in a few seconds and not ask questions.
- print the value on a line of its own, or near it.

| To read | Linux and AIX command | Windows (PowerShell) command |
|---|---|---|
| nginx version | `nginx -v 2>&1` | |
| Java version | `java -version 2>&1` | |
| Time zone | `date +%Z` | `(Get-TimeZone).Id` |
| A value in a file | `cat /etc/myapp/version` | `Get-Content C:\myapp\version.txt` |
| Installed package | `rpm -q --qf '%{VERSION}' openssl` | |

Some programs, such as `nginx -v` and `java -version`, print to the error
stream. KryoState reads only the standard output. Add `2>&1` to the end of
the command. On Windows, KryoState runs the command in PowerShell.

### Step 2: create your collectors file

Do this step only if you use the collectors file. Run this command one time:

```sh
kryostate collectors init
```

It creates `collectors.json` next to the KryoState database and shows where
it is. All KryoState commands and the web interface read your collectors from
this file. You do not have to give its name on the command line. The file
contains one working example:

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

Put each collector in the `specs` list. Put a comma between two collectors.
Do not put a comma after the last collector.

### Step 3: describe your collector

Open the file in a text editor and change the example into your collector.
Change `id`, `attr`, `os`, `command`, `parse` and `type`. The
[field reference](#every-field-for-reference) tells what each field means.
This is the full nginx collector:

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

`"optional": true` tells KryoState that some hosts do not have nginx. Without
it, KryoState records a host without nginx as a partly failed run.

Two collectors can record the same `attr` for different systems if their
`id` values are different. For example, add a Windows collector with the
command `$env:COMPUTERNAME` that records `host.nodename`.

### Step 4: choose how to read the output

The `parse` field takes the value from the output of the command. Find the
row that is like your output:

| Output | Write | Value |
|---|---|---|
| `UTC` (the full first line) | `{ "kind": "first_line" }` | `UTC` |
| `nginx version: nginx/1.24.0` (part of a line) | `{ "kind": "regex", "pattern": "nginx/([0-9.]+)" }` | `1.24.0` |
| `VERSION_ID="8.6"` (`NAME=value` lines) | `{ "kind": "kv", "key": "VERSION_ID" }` | `8.6` |
| `Version : 10.0.17763` (`Name : value` lines) | `{ "kind": "colon_kv", "key": "Version" }` | `10.0.17763` |
| `bos.rte  7.2.5.0  C  F  Base` (columns) | `{ "kind": "field", "marker": "bos.rte", "field": 2 }` | `7.2.5.0` |
| `{"app": {"version": "4.2"}}` (JSON) | `{ "kind": "json_path", "path": "app.version" }` | `4.2` |
| Many lines, all necessary (a small file) | `{ "kind": "all" }` | All lines, up to 8 KB |

The [field reference](#every-field-for-reference) tells how each kind
works. Use `first_line` if you can make the command print only the value.

**CAUTION:** In a JSON file, you must write each backslash two times. Write
the pattern `\d+` as `"\\d+"`. To prevent this problem, write `[0-9]`
instead of `\d`, and `[ ]` instead of `\s`.

### Yes-or-no answers

For a yes-or-no question, make the command print `true` or `false`, and set
`"type": "bool"`. The catalog collector **File exists** does this.

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

Do not print `yes` and `no`, `1` and `0`, or `installed` and `missing`. With
`true` and `false`, all hosts use the same words, and the charts show two
groups. The query `myapp.installed == true` finds the hosts where it is true.
The query `!myapp.installed` finds the other hosts.

### Step 5: test it

Test your collector on one host. KryoState saves nothing, so you can test
many times:

```sh
kryostate collectors try linux.nginx.version --host web01
```

KryoState connects, runs the command, and shows the result:

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

If the result is not correct, the message tells you why and what to try.

You can also test the parse rule without a host. Save the output from step 1
in a file. Then run:

```sh
kryostate collectors try linux.nginx.version --sample nginx-output.txt
```

Use this to get a regex correct. Change the pattern, run the command again,
and do this again until the result is correct.

### Step 6: collect

1. If you changed the collectors file and `kryostate serve` runs, restart
   `kryostate serve`.
2. Run `kryostate refresh`.

The value shows:

- on the page of each host, in the **Facts** table.
- in the inventory, if you add it as a column: **Columns** on the
  **Inventory** page, or `kryostate host list --cols nginx.version`.
- on the **Charts** page, when some hosts have it.

On the **Collectors** page, set the source filter to **your own** or
**added** to see your collectors. Each collector page shows the command, the
parse rule, a **Test** button and the **Schedule**. Your collectors run one
time each day by default while `kryostate serve` runs. To change this, use
the collector page, or run
`kryostate collectors schedule spec.linux.nginx.version --every 6h`.

If the collectors file has an error, the server starts, and the
**Collectors** page shows the error and where it is.

## When it does not work

| Message or problem | Cause | Action |
|---|---|---|
| `line 7, column 5: invalid character` | The file is not valid JSON. | Look before that position. Usually a comma is missing between two collectors, or there is a comma after the last one. |
| `unknown field "optinal"` | A field name has a spelling error. | Correct the spelling. KryoState refuses unknown fields, so that it does not ignore an error. |
| `duplicate collector id` or `is already defined` | Two collectors have the same `id`, or your `id` is a built-in `id`. | Give each collector a different `id`. |
| `the command failed: ... exited 127` | The program is not installed, or it is not on the `PATH`. | Use the full path, such as `/usr/sbin/nginx -v 2>&1`, or set `"optional": true`. |
| `the parse rule (regex) found nothing` | The command ran, but the parse rule did not match. | Compare the parse rule with the output. Test changes with `--sample`. |
| The output is empty. | The program printed to the error stream. | Add `2>&1` to the end of the command. |
| `NOTE: the host says it is ...` | The system of the host is not in `os`. | Add the system to the `os` list. |
| The value does not show after `refresh`. | KryoState did not find the file, the system is not in `os`, or the host is not in `stacks` or `where`. | Run `kryostate collectors list` and make sure that your collector is in the list. The collector page counts the hosts that it runs on, and shows a `where` that does not parse. |
| `"C:/Program Files/Git/etc/..." is not allowed` | Git Bash on Windows changed a path that starts with `/`. | Run `export MSYS_NO_PATHCONV=1` first, or use PowerShell or the web interface. |

## Going further

### A vendor application version

The catalog has no collectors for the products of one vendor. Many
applications write their version to a file in their install directory. In
this example, `/opt/acme/etc/version` contains `VERSION=4.2.1`, and only the
hosts of the `acme` stack have the application:

```json
{
  "id": "acme.version",
  "attr": "acme.version",
  "os": ["linux", "aix"],
  "stacks": ["acme"],
  "command": "cat /opt/acme/etc/version",
  "parse": { "kind": "kv", "key": "VERSION" },
  "type": "text",
  "optional": true
}
```

`kv` keeps the value after `VERSION=`. `stacks` limits the collector to the
members of the stack. Without `stacks`, it runs on all Linux and AIX hosts,
and `optional` prevents failed runs on hosts without the application. The
page of each member shows the collector under **Collectors on this host**,
with `collectors.json` as the reason.

### Comparing versions

Give a version an attribute name that ends in `.version`, such as
`nginx.version`. KryoState then groups the value by its first two numbers.
For example, the charts put `1.24.0` and `1.24.3` together under `1.24`. The
**Raw** column on the host page keeps the full value. This works for versions
with two, three or four numbers and dots, such as `1.24` or `9.4.1.2`.

If your program prints versions in a different format, such as `2024 R2`, add
a **normaliser rule**. When a normaliser rule reads some values of an
attribute, the values that no rule can read show as "not understood". Put
your normaliser rules in `rules.json` next to `collectors.json`. This rule
reads `2024 R2` as version 2024.2:

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

| Field | Meaning |
|---|---|
| `id` | A unique name for the normaliser rule |
| `attr` | The attribute name, `*` for all attributes, or a suffix such as `*.version` |
| `pattern` | A Go regular expression with named groups |
| `value` | The value that KryoState shows and uses in charts. `${name}` is a named group. |
| `family`, `major`, `minor`, `patch` | Optional. The parts of the version. |
| `comment` | Optional. A note for people. |

KryoState tries your normaliser rules before the built-in rules. After you
change a normaliser rule, run `kryostate renormalize` to apply it to the
values that KryoState collected before.

### Several copies of one program on a host

If a host runs two instances of a program, write one collector for each
instance. Give each collector a different `scope`, such as
`"scope": "instance-a"`. KryoState then keeps the two values. One value does
not replace the other.

### Fixing a parse rule after collecting

KryoState keeps the last output of each command. After you correct a `parse`
rule, apply it to this output. KryoState does not connect to a host for
this:

```sh
kryostate reparse --spec linux.nginx.version
```

### External programs

Some values are not on the host, such as the hypervisor cluster of the host,
its cloud tags, or its owner in a CMDB. A command on the host cannot read
them. For these values, write an **external program**. It is a program in a
language of your choice. It finds the host in a different system and prints
what it finds.

An external program runs on the KryoState server, not on the host. KryoState
runs it one time for each host:

1. KryoState starts the program. `KRYOSTATE_TARGET` contains the host name
   and `KRYOSTATE_ADDRESS` contains its address.
2. The program finds the host, for example in vCenter.
3. The program prints one JSON envelope of facts to standard output.
4. KryoState checks the envelope and records each fact as a value of the
   host.

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

| Envelope field | Meaning |
|---|---|
| `schema` | Must be `kryostate.facts/v1` |
| `collector` | The name of the program. Required. |
| `status` | `ok` or `failed` |
| `error` | Optional. Why the program failed. |
| `target` | Optional. The host that the facts are about. |
| `complete` | Optional. `true` if the facts are all the items of a category. |
| `facts` | Up to 500 facts. Each fact has `key`, `value`, and an optional `type`, `scope` and `observed_at`. |

Each `key` is an attribute name with at least one dot, 120 characters
maximum. Each `value` is 4096 characters maximum. The `type` is one of the
types in the [field reference](#every-field-for-reference). If one part of
the envelope is not correct, KryoState records none of it. The run then
shows as failed.

List the program in `externals.json`, in the same directory as the database
and `collectors.json`. Then restart `kryostate serve`.

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

| Field | Meaning |
|---|---|
| `name` | The name of the program. The collector is `external.<name>`. |
| `path` | The path of the program on the KryoState server |
| `args` | Optional. A list of arguments. |
| `timeout` | Optional. The maximum time, such as `30s`. The default is 60 seconds. |
| `env` | Optional. A list of `NAME=value` variables to give to the program |
| `comment` | Optional. A note that the **Collectors** page shows |

The program gets only `KRYOSTATE_TARGET`, `KRYOSTATE_ADDRESS`,
`KRYOSTATE_SCHEMA`, `PATH` and the variables in `env`. It gets no
credential, unless you put one in `env`. The **Collectors** page shows the
program with the source **external**.

## Every field, for reference

### Collector fields

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | A unique name. It must not be the `id` of a built-in collector. |
| `attr` | yes | The attribute name to record the value as |
| `os` | yes | A list of `linux`, `aix` and `windows` |
| `command` | yes | The command to run. It must only read. |
| `parse` | yes | The parse rule. See "Parse rules" below. |
| `type` | no | The type of the value. See "Value types" below. The default is `text`. |
| `optional` | no | `true` if some hosts do not have the value. Then a missing value is not a partly failed run. |
| `stacks` | no | A list of stack names. The collector runs only on members of these stacks. |
| `where` | no | A query, such as `stack == "web" && os.family == "Linux"`. The collector runs only on hosts that match. With `stacks`, a host must match the two. A query that does not parse runs on no host, and the collector page tells you why. |
| `scope` | no | A name that keeps the values of several instances on one host apart |
| `comment` | no | A note for people. KryoState does not use it. |

A collector runs on a host only if the system of the host is in `os`.
KryoState refuses fields that are not in this table.

A collectors file has two fields: `version`, which must be 1 or more, and
`specs`, which is the list of collectors.

### Parse rules

| `kind` | Other fields | What it keeps |
|---|---|---|
| `first_line` | none | The first line that is not blank, without spaces at the start and end. This is the default if `kind` is blank. |
| `regex` | `pattern` | The part of the output that a [Go regular expression](https://pkg.go.dev/regexp/syntax) matches: the group named `v` (`(?P<v>...)`), or else the first group in round brackets, or else the full match. Spaces at the start and end are removed. |
| `kv` | `key` | The value of the first `key=value` line with this key. The key must match exactly, with upper and lower case. Quotes around the value are removed. |
| `colon_kv` | `key` | The value of the first `key : value` line with this key, as PowerShell `Format-List` and `prtconf` print it. Upper and lower case are not important. |
| `field` | `marker`, `field` | Column `field` of the first line that contains `marker` and has that many columns. Spaces divide the columns. The first column is 1. |
| `json_path` | `path` | The value at `path` in a JSON document. `path` is names with dots, such as `app.version`. A JSON object or list is kept as JSON. |
| `all` | none | The full output, without blank lines at the start and end. KryoState cuts it at 8 KB. Use it only for small files. |

If the parse rule finds nothing, KryoState records a failure for the value.
A `regex` pattern that does not compile, or a missing `pattern`,
`key`, `marker` or `path`, is an error when KryoState loads the collector.
A `field` value less than 1 is also an error.

### Value types

| `type` | Use it for |
|---|---|
| `text` | Any text. This is the default. |
| `version` | A version number |
| `path` | A file or directory path |
| `number` | A number, such as a count or a size |
| `bool` | A yes-or-no value. The command prints `true` or `false`. |
| `list` | A list, such as `192.0.2.10,192.0.2.11` |

### Checks for `collectors add`

`kryostate collectors add` checks more than the collectors file does. It
refuses a collector if one of these is true:

- The `id` starts with `catalog.`, `builtin.`, `spec.` or `external.`.
- The `id` is the `id` of a built-in collector, a catalog entry or a
  collector in the collectors file.
- The `attr` is not lowercase words with dots, such as `app.version`.
- A value in `os` is not `linux`, `aix` or `windows`.
- The `type` is not one of the value types above.
- The `where` query does not parse.
- A stack in `stacks` does not exist.
