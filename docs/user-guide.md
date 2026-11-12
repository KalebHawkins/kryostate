# KryoState user guide

This guide is for people who are new to KryoState. It explains the ideas the
tool is built on, then walks through adding a host, putting hosts in a stack,
and adding a collector of your own. The guide is also served by the web
interface at `/guide`.

The [query guide](queries.md) is the reference for the query language, and
the [collector guide](collectors.md) explains writing collectors of your own.

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

Follow the tutorials below against it. Add the hosts `web01`, `web02`, `db01`
and `app01`, and use `kryostate` as the user. Or run `deploy/lab/lab.sh seed` to
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

Both connect to the host and run the built-in read-only commands for its
operating system, plus every collector you switched on. `refresh` also runs
the health checks (step 6). Leave
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

**In the web interface:** on the host's page, use **Application** and enter
the application.

**On the command line:**

```sh
kryostate set web01 application=payments
kryostate set web01 stack=payments-prod role=app -reason "per CHG-1042"
```

Anything else, such as `env=production`, is refused.

Every change goes into the audit log with your name. `-reason` adds an
optional note to that entry, such as a ticket number.

### Step 6 (optional): add a health check

A health check asks whether a service answers, separately from whether
KryoState can log in.

**In the web interface:** on the host's page, use **Health checks**. Choose
HTTP and enter a URL, or TCP and enter `host:port`.

**On the command line:**

```sh
kryostate check -host web01 -name https -http https://web01.example.com/health -expect 2xx
```

Checks run with `kryostate refresh`, or on their own with
`kryostate check -run`. **Collect now** in the web interface and scheduled
collections do not run them.

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

To add more hosts later, open the stack from the **Stacks** list and use **Add several hosts by name**.

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

- On the stack's page (open it from the **Stacks** list), type part of a host name in **Host** and pick it,
  optionally a **Role**, and select **Add host**.
- On a host's page, use **Stacks**. Typing the name of a stack that does not
  exist yet creates it.

**On the command line:**

```sh
kryostate stack -name payments-prod -add web01 -role app
```

### Every host a query matches

**In the web interface:** on the stack's page, open **Add hosts by query** and
write a query, as in the inventory's search box. As you type it shows how many
hosts it would add and the first few names, or where the query stops making
sense. Give a **Role** if they should have one and select the button, which
says how many it adds. Hosts already in the stack keep their role. It is a
one-off: hosts that match the query later are not added by themselves.

**On the command line:**

```sh
kryostate stack -name payments-prod -q 'os.family == "Linux" && name in ("web01", "web02")' -role app
```

### Look at it

The **Stacks** page shows each stack with its members, each member's role, and
counts: collected, failed and never collected. **Open in inventory** lists
the members with their values. Each host's page lists the collectors that run
on it, and why.

To collect something more from just this stack, switch it on from the
**Collectors** page with **Where it runs** set to the stack, or run
`kryostate collectors -enable group.web -stacks payments-prod`. That only ever
adds the stack: anything already collected on other hosts keeps running there.

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
DNS servers, SELinux mode, and the versions of Java, Python, OpenSSL, nginx,
Apache httpd and Docker. It also has fill-in-the-blank ones: **file exists**,
**file contents**, **a setting in a config file** such as `max_connections`
in PostgreSQL's `postgresql.conf`, **an environment variable** such as
`JAVA_HOME`, and **an installed package's version**.

**Groups** switch on several at once: **Host basics** (CPU, memory, time zone,
DNS), **Language runtimes** and **Web servers**.

**In the web interface:** open **Collectors**. Search or filter the list,
select a collector, and under **Where it runs** open **Switch on**. Leave
**Runs on** blank for every host, or write a query choosing the hosts, in the
same language as the **Inventory** search box: `stack == "web"`,
`name in ("web01", "web02")`, or `os.family == "Linux" && stack == "app"`.
The buttons under the box add a stack or a system, and **Add a host by
name** finds hosts as you type and adds them to a `name in (...)` list. A
preview counts the hosts the query matches and names the first few; nothing
ever lists every host. Select **Switch on**. (Groups are the buttons above
the list.) Then type part of a host name under **Test on a host**, pick one
of the suggestions and select **Test** to see what it reads. A test saves
nothing. You can also test from a host's own page, under **Test a
collector**, and switch things on for one stack from its card on the
**Stacks** page.

Each switched-on collector then shows where it runs, such as **Hosts in
web**, with **matches N hosts**, which opens the **Inventory** filtered to
those hosts. **Change where it runs** edits it. A collector with commands for
several systems runs the right one on each host by itself: you do not need a
query to separate Linux from Windows. The collector guide has more examples.

**On the command line:**

```sh
kryostate collectors -catalog
kryostate collectors -try memory.total -host web01
kryostate collectors -enable memory.total
kryostate collectors -enable group.web -stacks web
kryostate collectors -enable nginx.version -where 'name in ("web01", "web02")'
kryostate collectors -enable file.exists -param /etc/ssh/sshd_config.d/hardening.conf
kryostate collectors -enable config.value -param /etc/app/app.conf \
    -section database -key host
```

The values come back on each host's page with readable names, such as
**nginx version** or **host in app.conf [database]**. A version is grouped by
its first two numbers, so the charts put 1.22.1 and 1.22.4 together. The full version is in the **Raw** column, and
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

The easiest start is a collector that already exists. Open it on the
**Collectors** page and open **View definition**: it is written out exactly as
you would write it in your own file, with a **Copy** button.
`kryostate collectors -show <id>` prints the same.

```sh
kryostate collectors -init                                  # create the file
kryostate collectors -show cpu.cores                        # a worked example to copy
kryostate collectors -try example.nodename -host web01      # test, saves nothing
```

The **[collector guide](collectors.md)** explains the catalog in more detail
and walks through writing your own, with examples and a table of what to do
when something does not work.

## 7. Everyday use

### The daily loop

While `kryostate serve` runs, collection happens on its own (see
**Schedules** below). Open the **Overview** to see whether every host
collected, which hosts are failing and why, and what changed in the last day.

To collect everything straight away:

**In the web interface:** **Runs**, then **Collect now**.

**On the command line:**

```sh
kryostate refresh            # collect, then run the health checks
```

### Schedules

While `kryostate serve` runs, each collector runs on its own schedule: the
built-in operating system, hardware and identity groups, every catalog
collector you switched on, your own commands and external programs. Hardware,
versions and the operating system default to once a day; settings and file
checks to every 6 hours.

Scheduled runs are **throttled** so that they stay gentle on hosts that other
people look after:

- A scheduled run logs in only to the hosts that collector applies to, and
  runs only that collector's commands.
- At most 4 sessions are open at once across the estate, and 1 per host.
- Hosts are spread out at random over up to 2 minutes, so they are not all
  reached in the same second.
- A host that cannot be reached is left alone for 10 minutes, then 20, and
  so on, doubling each time up to 6 hours.
- **Collect now** waits for scheduled sessions to finish, and nothing
  scheduled starts while it runs.
- The shortest interval allowed is 5 minutes.

So collectors do run concurrently across hosts, but never more than 4
sessions at a time; the rest wait their turn. The **Schedules** page shows
these limits at the top.

**When a collector runs.** A schedule with a time of day (`-at 02:00`) runs
at exactly that time. A schedule without one, which includes every default,
runs at a time picked from the collector's name, the way Jenkins reads `H` in
a cron line: `builtin.os` might run daily at 16:04 and `builtin.hw` at 17:39,
every day, the same each day. An interval shorter than a day gets an offset
the same way, counted from midnight, so "every 6 hours" might mean 01:23,
07:23, 13:23 and 19:23. Without this, every collector a full collection had
just run would come due again in the same minute. A collector that has never
run is due straight away; after that it keeps to its own time. A slot less
than an hour after the last run (half the interval, for intervals under two
hours) is skipped, so an evening **Collect now** does not cause a second run
straight after.

**In the web interface:** **Schedules** is a timetable. A strip at the top
shows how many runs start in each of the next 24 hours, with busy hours
marked; select an hour to jump to it. Below it, collectors are listed soonest
first by clock time ("Today 14:00", "Tomorrow 02:00", "Thu 02:00", in the
server's time zone), and collectors whose runs start within 15 minutes of
each other are grouped under one heading with how many hosts they reach, how
many sessions they open and roughly how long those take, measured from
recent scheduled runs. A group that opens more than ten times the session
limit, or that has four or more collectors needing more sessions than can be
open at once, is marked **Busy slot**; when two or more of them run daily or
less often, **Spread these over 2h** gives each its own time across the two
hours from where the group starts.

To change one, select **Edit** on its row and pick how often it runs: the
default, **Manual only**, every 15 or 30 minutes, every 1, 2, 4, 6 or 12
hours, daily, every 2 days, weekly, or **Custom** for any other interval
such as `90m` or `3d`. For daily or longer, pick an hour and minute, or leave
the hour on **Picked for it**; **Use a quiet time** picks the quarter hour
with the fewest other runs near it. Then **Save**. Each collector's own page
has the same editor in its **Schedule** card.

**On the command line:**

```sh
kryostate collectors -schedules                               # list them, grouped by time
kryostate collectors -schedule builtin.os -every 1d -at 02:00
kryostate collectors -schedule cpu.cores -every 6h
kryostate collectors -schedule cpu.cores -manual              # or -default
```

Name a collector the way `-schedules` lists it: `builtin.os`, `builtin.hw` and
`builtin.host` for the built-in groups, a catalog name such as `cpu.cores`,
`spec.<id>` for one of your own, and `external.<name>` for an external
program. A running server picks up a change within a minute.

Start the server with `-no-schedule` to collect only when asked.

### Changes

A change is a collected value that differs from what the same collector read
before. **Changes** lists them for the last 24 hours, 7 days or 30 days; the
Overview shows the last day, and a host page marks a value that changed in
the last week with **changed** (hover for the old value).

### Rules: tracking a rollout

A rule follows a change across many hosts. You write two queries before
anything is changed: the **scope**, which hosts the rule covers, and the
**expect**, what each of them should report once it is done. Then, as hosts
are updated and collected again, they move from **Not yet** to **Done** on
their own. Nothing is ticked off by hand, and a rule never changes a host: the
counts come only from what hosts report.

Each host in scope is in one of three lists:

| List | Means |
|---|---|
| **Done** | It matches the expect query. |
| **Not yet** | Its values say it does not match yet, such as nginx 1.20 when 1.24 is expected. |
| **Unknown** | It has not reported a value the expect query needs, so it cannot be told either way. Usually the collector is not switched on, or has not run on it yet. The list says which value is missing. |

A rule is **complete** when every host in scope is done: none not yet and
none unknown. The badge says since when. A rule whose scope matches no host
is never complete.

#### Worked example: roll out the SSH hardening drop-in

Say every Linux host in the web stack should get
`/etc/ssh/sshd_config.d/hardening.conf`.

1. On **Collectors**, switch on **File exists** with the path
   `/etc/ssh/sshd_config.d/hardening.conf`. It records true or false on each
   host it runs on.
2. On **Rules**, open **New rule** and fill in:
   - Name: `SSH hardening drop-in`
   - Scope: `stack == "web" && os.family == "Linux"`
   - Expect: `file.exists["/etc/ssh/sshd_config.d/hardening.conf"] == true`

   The preview under each box shows how many hosts the scope covers and how
   they split, and points at the column of a typing mistake. A rule whose
   query does not parse is not saved.
3. Before the collector has run, every host is **Unknown**: "no value for
   file.hardening_conf.exists yet". After the next collection the hosts
   without the file are **Not yet** and those with it **Done**.
4. Push the file out with whatever you use for that. Each collection moves
   hosts into **Done**, and the rule's trend line climbs. The **Not yet** list
   links to the inventory with exactly those hosts, for the next batch.

The same works for a version (expect `nginx.version >= "1.24"`, scope
`stack == "web"`) or a setting (expect
`conf.app_conf.database.host == "db02"`, after switching on **Config file
value** for that key).

The **Rules** page lists every rule with a progress bar, a small trend line
and when its counts last moved; the Overview shows open rules under
**Rollouts**. A rule's own page lists the hosts in each bucket (fifty of each,
with a link to the inventory for the rest) and a chart of how the counts
moved. A point is kept whenever a collection run finishes with different
counts, and once a day otherwise. Changing a rule's queries starts its trend
again, since the old points answered another question.

From the command line:

```sh
kryostate rules                                   # every rule, with counts
kryostate rules -add -name 'nginx 1.24' -scope 'stack == "web"' -expect 'nginx.version >= "1.24"'
kryostate rules -show 'nginx 1.24'                # the hosts not yet done, and the unknown ones
kryostate rules -show 'nginx 1.24' -json          # every host in scope, by list
kryostate rules -delete 'nginx 1.24'
```

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
Prometheus label matcher. The inventory shows the hosts for which it is true.

```text
os.version == "AlmaLinux 9.8"
os.family == "Linux" && !file.exists == "/etc/ssh/sshd_config.d/hardening.conf"
stack == "data" && role == "database"
name in ("web01", "web02")
hw.cpu.cores >= 8 && nginx.version < "1.24"
!has(os.version)
```

Compare with `==` `!=`, match patterns with `=~` `!~`, order numbers and
versions with `<` `<=` `>` `>=`, test a list with `in`, and join with `&&`,
`||`, `!` and parentheses. Names are attributes such as `os.version`, plus
`name`, `id`, `stack`, `role` and `application`. Text with none of the query
symbols in it, such as `web`, searches host names.

The same queries choose where a collector runs (section 6), so a query that
lists the right hosts here is the one to give a collector.

The [query guide](queries.md) is the full reference: every operator, how
missing values, versions and several stacks compare, collectors that take a
path, recipes and common mistakes. In the web interface it is at
**Guide → Queries** (`/guide/queries`).

#### JSON export

`/hosts.json` takes the same `q` and `col` parameters as the Inventory page,
and `kryostate hosts -json` prints the same shape:

```json
[
  {
    "id": "01J9...",
    "name": "web01",
    "stacks": [{"stack": "web", "role": "web"}],
    "application": "nginx",
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
| Overview | One line saying whether every host collected; the hosts that need attention (failing, no connection, out of date, never collected), each linking to the inventory filtered to them; the last day's changes; the next runs by clock time; hosts by operating system, stack and application | |
| Inventory | One row for each host, filtered by a query | Add a host; choose columns; download CSV or JSON |
| Host page | Values, sources, reachability, identity, addresses | Connection, stacks, health checks, application, retire and restore, chosen address; test any collector on it |
| Stacks | One card for each stack, with roles and what is collected there | Create, edit and delete stacks; add and remove members; switch collectors on for one stack |
| Charts | One chart for each attribute: a donut for a few values, ranked bars for more, a histogram for numbers. Attributes that identify a host are listed, not charted. Open a chart for a larger view and a table; each value links to its hosts | |
| Changes | Collected values that changed, by time window | |
| Runs | The collection history, manual and scheduled | Start a collection |
| Schedules | When each collector runs next, grouped by time, with a 24-hour timeline and busy slots marked | Change how often and when each collector runs; spread a busy slot out |
| Collectors | Every collector: built-in, catalog, your own and external programs, with a search box and filters, and the groups | Switch a group on |
| Collector page | One collector: what it records, where it runs, its commands, its **Schedule**, and **View definition** (the collector as it would be written in `collectors.json`) | Switch a catalog collector on or off and choose the hosts it runs on with a query and a live count, test it on a host found by name, change its schedule |
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
| External program | A program you write that runs on the KryoState server, not on the host, once for each host. It is given the host in `KRYOSTATE_TARGET` and `KRYOSTATE_ADDRESS` and prints a JSON envelope of facts that the host cannot report itself, such as its hypervisor cluster or its owner in a CMDB. Listed in `externals.json` next to the database |
| Change | A collected value that differs from what the same collector read before |
| Normaliser | A rule that turns a raw string into a comparable value |
| Query | A condition, such as `os.family == "Linux"`, that picks hosts in the inventory |
| Schedule | How often a collector runs on its own while `kryostate serve` runs |
| Scope | Separates several instances of the same thing on one host |
| Stack | A named group of hosts that make up one deployed system |
