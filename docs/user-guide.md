# KryoState user guide

This guide is for people who are new to KryoState. It explains the ideas the
tool is built on, then walks through adding a host, putting hosts in a stack,
and adding a collector of your own. The guide is also served by the web
interface at `/guide`.

For the reasoning behind each design decision, read the files in
`docs/adr/`. For the security assumptions, read `docs/threat-model.md`.

## 1. What KryoState does

Most estates have a written inventory: a wiki page, a spreadsheet, or a
Markdown table. Over time it drifts away from what is really installed.
KryoState takes from you, or from such a document, only what a machine cannot
say about itself: which hosts exist, which stacks they belong to, the role each
plays there and the application it runs. It connects to each host to read
everything else from the host itself. What a host reports is the truth for
that attribute.

## 2. Five ideas to know first

### 2.1 Every value is a claim

KryoState stores no "current value" for anything. Each value is a **claim**
(the code calls it an assertion) with five parts:

| Part | Example |
|---|---|
| Host | `web01` |
| Attribute | `os.version` |
| Value | `RHEL 8.6` |
| Source | `ssh` (a collector), or for an application a person or `inventory.md` |
| Time | when the source said it |

Claims are never overwritten. When a value changes, the old claim stays in the
database beside the new one, so the history of each host is kept.

### 2.2 The host always wins

What a host reports wins over every other source, on every attribute. Where no
collector has said anything, the value with the best rank is shown. A lower
number wins.

| Source kind | Rank | Where it comes from |
|---|---|---|
| collector | first | A command run on the host |
| human | 20 | A person, with `kryostate set` or the host page (application only) |
| dns | 30 | A DNS lookup |
| document | 40 | An imported document (application only) |
| derived | 90 | Inferred from another value |

### 2.3 What a person may state

A person or a document may state a host's **application**, and put it in
**stacks** with a **role** in each. Nothing else is entered by hand: the
operating system, versions and every other attribute come from the host.

### 2.4 Hosts have an opaque id

Each host has a random id. Its name is evidence of who it is, not its key,
because names are reused. On the command line you can still name a host by
its name, its fqdn, or the first six characters of its id. If a name matches
two hosts, KryoState stops and lists both.

### 2.5 Unknown is not blank

A host with no value for an attribute is put in one of three groups:

| Group | Meaning | What to do |
|---|---|---|
| never collected | Nothing has asked this host | Add a connection profile |
| collection failed | The last attempt failed | Repair the access |
| not understood | The host answered and no rule read it | Fix or add a rule |

## 3. Installation

You need Go 1.27.1 or later (1.27.2 or later is better).

```sh
go build ./cmd/kryostate
```

The result is one binary. It contains the web interface and needs no other
files.

The database is a single SQLite file. By default it is in your user
configuration directory. To use a different file, set `KRYOSTATE_DB` or pass
`-db` to any command:

```sh
export KRYOSTATE_DB=$HOME/kryostate/estate.db
```

Your name is written to the audit trail. KryoState reads it from
`KRYOSTATE_ACTOR`, then `USER`, then `USERNAME`.

### Trying it without your own servers

If you have Podman (or Docker with `docker` in place of `podman`), the repository
has a lab: KryoState, an ssh-agent, and four small Linux hosts to collect from.

```sh
deploy/lab/lab.sh up      # kryostate is then on http://localhost:8099
```

Follow the tutorials below against it. Add the hosts `uf01`, `uf02`, `idx01`
and `web01`, and use `kryostate` as the user. Or run `deploy/lab/lab.sh seed` to
have everything set up for you, and look around. Run commands inside the
container with `podman exec -i kryostate-lab kryostate <command>`.

## 4. Tutorial: add your first host

This tutorial adds one Linux host, collects from it, and reads the result.
Every step can be done in the web interface or on the command line; both
are shown. Replace `web01.example.com` and `svc-ro` with a real host and a
real read-only account.

### Step 0: start the web interface

```sh
kryostate serve -addr 127.0.0.1:8099
```

Open `http://127.0.0.1:8099` in a browser. The web interface has no login of
its own, so keep it on `127.0.0.1`, or put it behind a proxy that asks people
to log in (section 8).

### Step 1: record the host

**In the web interface:** open **Inventory**, then **Add a host**. Enter the
name and, if you know it, the fully qualified name. Select **Add host**. You
are taken to the host's page.

**On the command line:**

```sh
kryostate host web01 -fqdn web01.example.com
```

Give the fully qualified name when you know it. It is stronger evidence of
which machine this is than a short name, which may repeat between sites.

### Step 2: tell KryoState how to reach it

KryoState logs in to a host over SSH to read it. It needs an address, a user
name, and a credential.

**In the web interface:** on the host's page, find **Manage this host**, then
**Connection**. The address is filled in from the fully qualified name. Enter
the user and select **Add connection**.

The web interface always uses your **ssh-agent** as the credential. The
private key never leaves the agent, so it cannot be exposed through the web
page. Make sure the agent is running and holds the key, on the machine that
runs `kryostate serve`.

**On the command line:**

```sh
kryostate target -host web01 -user svc-ro
```

The command line can also use other credentials: `keyring:service/account`
(the operating system's credential store), `file:/path/key`,
`exec:/path/helper` and `env:VAR`, with `-credential`. Windows hosts use
`-transport winrm`, which is HTTPS only. Connections that use these can only
be changed on the command line, because a password could otherwise be sent to
another machine from the web page.

The database stores a credential **reference**, never the key or password.

### Step 3: collect

**In the web interface:** open **Runs** and select **Collect now**.
Progress appears for each host as it finishes.

**On the command line:**

```sh
kryostate refresh -only web01
```

Both connect to the host, run the built-in read-only commands for its
operating system, and run any health checks. Leave
out `-only` to do every host.

If the connection fails, the reason is shown. The most common causes are an
ssh-agent that is not running and a host key that has changed.

### Step 4: read the result

**In the web interface:** open the host's page. The **Facts** table shows
every value, where it came from, and when.

**On the command line:**

```sh
kryostate show web01
```

### Step 5: state what a machine cannot know

A host cannot say what application it runs. You can. This is the only
attribute you enter by hand; its stacks and role are covered in section 5.

**In the web interface:** on the host's page, use **Application**. Enter the
application and a reason.

**On the command line:**

```sh
kryostate set web01 application=payments -reason "per CHG-1042"
kryostate set web01 stack=payments-prod role=app -reason "per CHG-1042"
```

Anything else, such as `env=production`, is refused.

The reason is required. It goes into the audit log with your name.

### Step 6 (optional): add a health check

A health check asks whether a service answers, separately from whether
KryoState can log in.

**In the web interface:** on the host's page, use **Health checks**. Choose
HTTP and enter a URL, or TCP and enter `host:port`.

**On the command line:**

```sh
kryostate check -host web01 -name https -http https://web01.example.com/health -expect 2xx
```

Checks run with every collection.

## 5. Tutorial: build a stack

A **stack** is a deployed system, such as "the payments service in
production". A host can belong to more than one stack, and plays a **role**
in each, such as `app` or `database`. KryoState does not discover stacks; you
say which hosts belong.

### The fast way: paste the list

If you have the hosts in a spreadsheet or a wiki page, you can create the
stack, its hosts, their roles and their connections in one step.

**In the web interface:** open **Stacks**, then **New stack**. Enter a name
such as `payments-prod`, and optionally the environment, owner and
description. In **Hosts**, put one host per line: the name, then its role.

```text
web01 app
web02 app
db01.example.com database
lb01 loadbalancer lb01.example.com
```

A name with dots in it is taken as the fully qualified name, and may come
first or last. Words may be separated by spaces, tabs or commas, so columns
copied from a spreadsheet work. Hosts that KryoState does not know yet are
added. Fill in **Connect new hosts as user** to give each new host a
connection that logs in as that user with your ssh-agent. Fill in
**Application** to record the application every listed host runs. Select
**Create stack**.

To add more hosts later, use **Add several hosts** in the stack's card.

**On the command line:** put the same lines in a file, then:

```sh
kryostate stack -name payments-prod -environment production \
    -owner payments-team -file hosts.txt -create -user svc-ro -application payments
```

`-create` adds hosts that are not recorded yet; without it, an unknown name is
reported and skipped. A short list can go straight on the command line:
`-add web01:app,db01:database`.

### One host at a time

**In the web interface:**

- On the **Stacks** page, in the stack's card, type a host name in **Host**,
  optionally a **Role**, and select **Add host**.
- On a host's page, use **Stacks**. Typing the name of a stack that does not
  exist yet creates it.

**On the command line:**

```sh
kryostate stack -name payments-prod -add web01 -role app
```

### Look at it

The **Stacks** page shows each stack with its members, each member's role, and
counts: collected, failed and never collected. **Collected
here** lists what is read from the stack's hosts. **Open in inventory** lists
the members with their values.

To collect something more from just this stack, pick it under **Also collect
on**, for example **Splunk Universal Forwarder version** on your Splunk
stack, and select **Switch on**. That only ever adds the stack: anything already
collected on other hosts keeps running there.

On the command line, `kryostate stack` lists the stacks and
`kryostate hosts -stack payments-prod` lists the members.

### Changing a stack

**In the web interface:** each member has a **Remove** button. **Edit
details** changes the environment, owner and description, and has **Delete
stack**. Deleting a stack does not change its hosts.

**On the command line:**

```sh
kryostate stack -name payments-prod -remove lb01
kryostate stack -name payments-prod -owner platform-team
```

Flags you leave out keep their current values.

### Importing stacks from a document

If your inventory already has a stack column, the importer creates the stacks
for you:

```sh
kryostate import -name inventory.md inventory.md
```

An inventory supplies the hosts, their names and addresses, the stack each
belongs to, its role there, and the application it runs. Nothing else is
imported: the operating system, versions and the rest come from the hosts.

KryoState reads the column headings and prints what it understood. It
recognises common names: Host or Hostname, FQDN, Address or IP, Stack, Role and
Application. It lists every other column as not imported, so a column such as
OS or Owner is named rather than dropped without a word. If your headings use
other names, write a mapping file that says which column means what:

```json
{
  "host_column": "Server",
  "fqdn_column": "DNS name",
  "address_column": "IP",
  "stack_column": "System",
  "role_column": "Function",
  "application_column": "Product"
}
```

and pass it with `-map`:

```sh
kryostate import -name inventory.md -map mapping.json inventory.md
```

The `-name` is the source name shown beside every application from this
file. Import the same file again after you edit it; unchanged values are not
duplicated, and stack details you set by hand are kept. A CSV file works the
same way.

## 6. Tutorial: choose what to collect

KryoState always reads the operating system, its version, the kernel, the
hardware model and the serial number. Anything else, you switch on. A
**collector** reads one value from each host.

### Step 1: pick from the catalog

The catalog has ready-made collectors for CPU cores, memory size, time zone,
DNS servers, SELinux mode, the Splunk forwarder and Splunk Enterprise
versions, Splunk's install location, and the versions of Java, Python,
OpenSSL, nginx, Apache httpd and Docker. It also has fill-in-the-blank ones:
**file exists**, **file contents**, **a setting in a config file** such as
`serverName` in Splunk's `server.conf`, **an environment variable** such as
`SPLUNK_HOME`, and **an installed package's version**.

**Groups** switch on several at once: **Host basics** (CPU, memory, time zone,
DNS), **Language runtimes** and **Web servers**. The Splunk collectors are
switched on one by one.

**In the web interface:** open **Collectors**. Search or filter the list,
select a collector, and under **Where it runs** open **Switch on**, choose
**All hosts** or only some stacks, and select **Switch on**. (Groups are the
buttons above the list.) Then choose a host next to it and select
**Test** to see what it reads. A test saves nothing. You can also test from a
host's own page, under **Test a collector**, and switch things on for one
stack from its card on the **Stacks** page.

**On the command line:**

```sh
kryostate collectors -catalog
kryostate collectors -try memory.total -host web01
kryostate collectors -enable memory.total
kryostate collectors -enable splunk.uf.version -stacks splunk-prod
kryostate collectors -enable file.exists -param /etc/resolv.conf
kryostate collectors -enable config.value -param /opt/splunkforwarder/etc/system/local/server.conf \
    -section general -key serverName
```

The values come back on each host's page with readable names, such as
**Splunk Universal Forwarder version** or **serverName in server.conf
[general]**. A version is grouped by its first two numbers, so the charts put
9.4.1 and 9.4.3 together. The full version is in the **Raw** column, and
`kryostate show` prints it in brackets.

### Step 2: collect

Wait for its schedule (see **Schedules**), select **Collect now** on the
**Runs** page, or run `kryostate refresh`.
The new values appear on each host's page. To see one for every host, open
**Inventory**, then **Columns**, and tick it.

### Step 3 (only if nothing fits): write your own

Any read-only command can become a collector. You write the command, a rule
for which part of its output to keep, and a name, in a file on the KryoState
server. You do not need to write a program.

```sh
kryostate collectors -init                                  # create the file
kryostate collectors -try example.nodename -host web01      # test, saves nothing
```

The **[collector guide](collectors.md)** explains the catalog in more detail
and walks through writing your own, with examples and a table of what to do
when something does not work.

## 7. Everyday use

### The daily loop

**In the web interface:** **Runs**, then **Collect now**.

**On the command line:**

```sh
kryostate refresh            # collect and check
```

### Schedules

While `kryostate serve` runs, each collector runs on its own schedule: the
built-in operating system, hardware and identity groups, every catalog
collector you switched on, your own commands and external programs. Hardware,
versions and the operating system default to once a day; settings and file
checks to every 6 hours. A scheduled run logs in only to the hosts that
collector applies to and runs only its commands. It opens at most 4 sessions
at once and 1 per host, spreads hosts over up to 2 minutes, and leaves a host
it cannot reach alone for 10 minutes, doubling each time up to 6 hours. A
**Collect now** waits for scheduled sessions to finish, and nothing scheduled
starts while it runs.

**In the web interface:** **Schedules** lists each collector with its last
run and next due time. Type an interval such as `30m`, `6h` or `1d`, and for
whole days optionally a time, then **Save**; or choose **Manual only**.

**On the command line:**

```sh
kryostate collectors -schedules                               # list them
kryostate collectors -schedule builtin.os -every 1d -at 02:00
kryostate collectors -schedule cpu.cores -every 6h
kryostate collectors -schedule cpu.cores -manual              # or -default
```

Start the server with `-no-schedule` to collect only when asked.

### Changes

A change is a collected value that differs from what the same collector read
before. **Changes** lists them for the last 24 hours, 7 days or 30 days; the
Overview shows the last day, and a host page marks a value that changed in
the last week with **changed** (hover for the old value).

### Finding hosts

On the **Inventory** page, type part of a name or a query into the box,
choose columns, and download the view as CSV or JSON. The query is kept in
the address (`/hosts?q=...`), so a filtered view can be bookmarked or pasted
to someone else. On the command line:

```sh
kryostate hosts                          # every host
kryostate hosts pay                      # names that contain "pay"
kryostate hosts -q 'os.family == "Linux" && stack == "web"'
kryostate hosts -cols os.version,application
kryostate hosts -json                    # for scripts
```

#### Queries

A query is a condition on each host's current values, in the style of a
Prometheus label matcher:

```text
os.version == "Red Hat 7.9 (Ootpa)"
os.family == "Linux" && !file.exists == "/opt/splunkforwarder/bin/splunk"
stack == "splunk-prod" && role == "indexer"
name =~ "^web" || application == "nginx"
hw.cpu.cores >= 8 && kernel.version < "4"
!has(os.version)
```

| Write | Means |
|---|---|
| `==` `!=` | equal, not equal. Text is exact and case sensitive. |
| `=~` `!~` | matches, does not match a Go regular expression. Patterns are **not anchored**: `"web"` matches anywhere in the value, `"^web"` only at the start. `(?i)` ignores case. |
| `<` `<=` `>` `>=` | numbers compare as numbers (`8 < 16`), versions in version order (`"7.9" < "7.10"`). A value that is neither does not match. |
| `&&` `\|\|` `!` `( )` | and, or, not, grouping. `&&` binds tighter than `\|\|`; `!` applies to the whole comparison after it. |
| `"text"` `'text'` | a string; `\"`, `\\`, `\n` and `\t` are escapes. A pattern full of backslashes is easier as a raw string in backquotes, which has no escapes. |
| `true` `false` `8` `-1.5` | literals. `true` matches a value of true or yes, `false` one of false or no. |
| `has(x)` | the host has a value for x. So does `x != ""`; `x == ""` finds hosts without one. |
| `x` alone | the same as `x == true`, for yes/no attributes. |

Names are attribute names such as `os.version`, plus a few that are not
attributes: `name` (the host's name), `id`, `stack` (any stack it is in),
`role` (its role in any stack, or its `role` attribute) and `application`
(its `app.name`). `stack != "x"` means "not in stack x".

A collector that takes a path or a name records an attribute named after it,
such as `file.splunk.exists` for `/opt/splunkforwarder/bin/splunk`. You can use
that name, or name the collector and its parameter:
`file.exists["/opt/splunkforwarder/bin/splunk"] == true`. The shorter
`file.exists == "/opt/splunkforwarder/bin/splunk"` means the same, and
`package.version == "openssl"` means the openssl version collector has a
value. Text with none of the query symbols in it is a name search, as before.

A query that does not parse is shown with the column where the problem is.
Queries are evaluated in KryoState over the loaded values; none of it is
passed to the database.

#### JSON export

`/hosts.json` takes the same `q` and `col` parameters as the Inventory page,
and `kryostate hosts -json` prints the same shape:

```json
[
  {
    "id": "01J9...",
    "name": "web01",
    "stacks": [{"stack": "splunk-prod", "role": "indexer"}],
    "application": "splunk",
    "values": {"os.version": "RHEL 8.6", "env": null},
    "last_collected": "2026-11-12T08:00:00Z",
    "collect_ok": true
  }
]
```

`values` holds the chosen columns, `null` where the host has no value; `col=*`
chooses every attribute, and the command line includes every value unless
`-cols` is given. `application` and `last_collected` are left out when there
is none.

### Retiring a host

When a machine is gone, retire it. Its history is kept and collection stops.

**In the web interface:** on the host's page, **Retire**, with a reason. To
find it again, use **Retired hosts** on the Inventory page; its page has
**Restore host**.

**On the command line:**

```sh
kryostate host -retire web01 -reason "decommissioned, CHG-2210"
kryostate host -unretire web01
```

### Backing up

```sh
kryostate export -out backup.json
kryostate restore -db new.db -in backup.json
```

`restore` needs an empty database.

## 8. The web interface

| Page | What it shows | What you can change |
|---|---|---|
| Overview | Whether hosts collected, failing hosts and why, the last day's changes, what runs next | |
| Inventory | One row for each host, filtered by a query | Add a host; choose columns; download CSV or JSON |
| Host page | Values, sources, reachability, identity, addresses | Connection, stacks, health checks, application, retire and restore, chosen address; test any collector on it |
| Stacks | One card for each stack, with roles and what is collected there | Create, edit and delete stacks; add and remove members; switch collectors on for one stack |
| Charts | One chart for each attribute: a donut for a few values, ranked bars for more, a histogram for numbers. Attributes that identify a host are listed, not charted. Open a chart for a larger view and a table; each value links to its hosts | |
| Changes | Collected values that changed, by time window | |
| Runs | The collection history, manual and scheduled | Start a collection |
| Schedules | Each collector's schedule, last run and next due time | Change how often each collector runs |
| Collectors | Every collector that will run, the groups and the catalog | Switch catalog collectors and groups on and off, choose their stacks, test one on a host |
| Guide | This document | |

Every change is written to the audit log with the name of the person who
made it. After a change, a message at the top of the page says what
happened, or why it was not saved.

Every filter is in the URL. Copy the address bar to share exactly what you
see.

### Security

The web interface has no login of its own. Either keep it on `127.0.0.1`, or
put it behind a proxy that authenticates people and passes their name in the
`X-Forwarded-User` header; KryoState records that name.

Forms only accept submissions from the KryoState page itself, so another web
site cannot make changes through your browser. If a proxy serves KryoState
under a different name than the one it forwards, list that name when
starting:

```sh
kryostate serve -trusted-origin https://cmdb.example.com
```

## 9. Troubleshooting

**"no agent is listening"**: start ssh-agent and add your key. On Windows,
start the "OpenSSH Authentication Agent" service.

**"host key does not match the recorded key"**: the host presented a
different key from the one KryoState pinned the first time. Either the host was
rebuilt, or something is pretending to be it. KryoState will not connect until
you decide. Compare the fingerprint in the message with the host's own (on
the host: `ssh-keygen -lf /etc/ssh/ssh_host_ecdsa_key.pub`). If they match,
clear the old key, and the next collection pins the new one:

```sh
kryostate target -host web01 -forget-host-key
```

This is on the command line only, and is written to the audit log.

**"matches 2 items"**: two hosts share that name. Use the fqdn or the id that
the error lists.

**A value shows "not understood"**: the host answered, but no rule read the
answer. Look at the raw output with `kryostate evidence -host <name> -show`,
then fix the parse rule (`reparse`) or the normaliser rule (`renormalize`).

**My collector did not run**: run `kryostate collectors` and check that it is
listed, then test it with `kryostate collectors -try <id> -host <name>`. The
[collector guide](collectors.md#when-it-does-not-work) lists the common causes.

**A host shows "never" collected**: it has no enabled connection. Add one on
the host's page, under **Connection**.

**"Not saved: this connection uses a credential the web interface cannot
change"**: the connection uses a password or a helper program. Change it with
`kryostate target`.

**A form answers "403 Forbidden"**: the browser reached KryoState under a
different name than the proxy forwards. Start it with
`-trusted-origin https://<the name in the address bar>`.

**A path is refused as "C:/Program Files/Git/..."**: Git Bash on Windows
rewrites arguments that start with `/`. Run `export MSYS_NO_PATHCONV=1`
first, or use PowerShell or the web page.

## 10. Glossary

| Term | Meaning |
|---|---|
| Assertion | One claim by one source about one attribute of one host |
| Attribute | The name of a value, such as `os.version`. You can make up new ones |
| CI | Configuration item. A host is one kind |
| Collector | Something that reads a value from a host |
| Connection profile | How to reach a host: transport, address, user, credential reference |
| Evidence | The stored output of a collector command |
| Normaliser | A rule that turns a raw string into a comparable value |
| Scope | Separates several instances of the same thing on one host |
| Stack | A named group of hosts that make up one deployed system |
