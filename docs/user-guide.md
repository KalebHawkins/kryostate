# hearsay user guide

This guide is for people who are new to hearsay. It explains the ideas the
tool is built on, then walks through adding a host, putting hosts in a stack,
and adding a collector of your own. The guide is also served by the web
interface at `/guide`.

For the reasoning behind each design decision, read the files in
`docs/adr/`. For the security assumptions, read `docs/threat-model.md`.

## 1. What hearsay does

Most estates have a written inventory: a wiki page, a spreadsheet, or a
Markdown table. Over time it drifts away from what is really installed.
hearsay records what your documents claim about each host, connects to each
host to read what the host itself reports, and shows you where the two do not
agree.

hearsay does not trust either side by default. It keeps both statements and
lets you decide.

## 2. Five ideas to know first

### 2.1 Every value is a claim

hearsay stores no "current value" for anything. Each value is a **claim**
(the code calls it an assertion) with five parts:

| Part | Example |
|---|---|
| Host | `web01` |
| Attribute | `os.version` |
| Value | `RHEL 8.6` |
| Source | `inventory.md` (a document) or `ssh` (a collector) |
| Time | when the source said it |

Claims are never overwritten. When a document and a host disagree, both
claims stay in the database.

### 2.2 Sources have a rank

When sources disagree, the value with the best rank is shown. A lower number
wins.

| Source kind | Rank | Where it comes from |
|---|---|---|
| collector | 10 | A command run on the host |
| human | 20 | A person, with `hearsay set` or the host page |
| dns | 30 | A DNS lookup |
| document | 40 | An imported document |
| derived | 90 | Inferred from another value |

For `env`, `site` and `owner`, a person ranks first, because a machine cannot
know what environment it belongs to.

### 2.3 A disagreement is a finding

When two sources disagree, `hearsay detect` raises a **disagreement**. You
settle each one with one of three decisions:

- **Machine wins**: the document is out of date. The host's value stands.
- **Document wins**: the collector read the wrong thing. The document's value
  stands.
- **Accept**: the difference is known and tolerated.

Other kinds of finding are also raised: a value no rule understood, a value
that is too old, and a value that the documents never mention.

### 2.4 Hosts have an opaque id

Each host has a random id. Its name is evidence of who it is, not its key,
because names are reused. On the command line you can still name a host by
its name, its fqdn, or the first six characters of its id. If a name matches
two hosts, hearsay stops and lists both.

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
go build ./cmd/hearsay
```

The result is one binary. It contains the web interface and needs no other
files.

The database is a single SQLite file. By default it is in your user
configuration directory. To use a different file, set `HEARSAY_DB` or pass
`-db` to any command:

```sh
export HEARSAY_DB=$HOME/hearsay/estate.db
```

Your name is written to the audit trail. hearsay reads it from
`HEARSAY_ACTOR`, then `USER`, then `USERNAME`.

## 4. Tutorial: add your first host

This tutorial adds one Linux host, collects from it, and reads the result.
Every step can be done in the web interface or on the command line; both
are shown. Replace `web01.example.com` and `svc-ro` with a real host and a
real read-only account.

### Step 0: start the web interface

```sh
hearsay serve -addr 127.0.0.1:8099
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
hearsay host web01 -fqdn web01.example.com
```

Give the fully qualified name when you know it. It is stronger evidence of
which machine this is than a short name, which may repeat between sites.

### Step 2: tell hearsay how to reach it

hearsay logs in to a host over SSH to read it. It needs an address, a user
name, and a credential.

**In the web interface:** on the host's page, find **Manage this host**, then
**Connection**. The address is filled in from the fully qualified name. Enter
the user and select **Add connection**.

The web interface always uses your **ssh-agent** as the credential. The
private key never leaves the agent, so it cannot be exposed through the web
page. Make sure the agent is running and holds the key, on the machine that
runs `hearsay serve`.

**On the command line:**

```sh
hearsay target -host web01 -user svc-ro
```

The command line can also use other credentials: `keyring:service/account`
(the operating system's credential store), `file:/path/key`,
`exec:/path/helper` and `env:VAR`, with `-credential`. Windows hosts use
`-transport winrm`, which is HTTPS only. Connections that use these can only
be changed on the command line, because a password could otherwise be sent to
another machine from the web page.

The database stores a credential **reference**, never the key or password.

### Step 3: collect and compare

**In the web interface:** open **Overview** and select **Collect now**.
Progress appears for each host as it finishes.

**On the command line:**

```sh
hearsay refresh -only web01
```

Both connect to the host, run the built-in read-only commands for its
operating system, run any health checks, and then compare the sources. Leave
out `-only` to do every host.

If the connection fails, the reason is shown. The most common causes are an
ssh-agent that is not running and a host key that has changed.

### Step 4: read the result

**In the web interface:** open the host's page. The **Facts** table shows
every value, where it came from, and when.

**On the command line:**

```sh
hearsay show web01
```

Where sources disagree, every competing claim is listed under the value that
currently wins.

### Step 5: state what a machine cannot know

A host cannot report its environment or owner. You can.

**In the web interface:** on the host's page, use **State a value**. Enter the
attribute (such as `env`), the value, and a reason.

**On the command line:**

```sh
hearsay set web01 env=production owner=payments-team -reason "per CHG-1042"
```

The reason is required. It goes into the audit log with your name.

### Step 6 (optional): add a health check

A health check asks whether a service answers, separately from whether
hearsay can log in.

**In the web interface:** on the host's page, use **Health checks**. Choose
HTTP and enter a URL, or TCP and enter `host:port`.

**On the command line:**

```sh
hearsay check -host web01 -name https -http https://web01.example.com/health -expect 2xx
```

Checks run with every collection.

## 5. Tutorial: build a stack

A **stack** is a deployed system, such as "the payments service in
production". A host can belong to more than one stack, and plays a **role**
in each, such as `app` or `database`. hearsay does not discover stacks; you
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
copied from a spreadsheet work. Hosts that hearsay does not know yet are
added. Fill in **Connect new hosts as user** to give each new host a
connection that logs in as that user with your ssh-agent. Select **Create
stack**.

To add more hosts later, use **Add several hosts** in the stack's card.

**On the command line:** put the same lines in a file, then:

```sh
hearsay stack -name payments-prod -environment production \
    -owner payments-team -file hosts.txt -create -user svc-ro
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
hearsay stack -name payments-prod -add web01 -role app
```

### Look at it

The **Stacks** page shows each stack with its members and counts: collected,
failed, never collected, and open disagreements. **Open in inventory** lists
the members with their values.

On the command line, `hearsay stack` lists the stacks and
`hearsay hosts -stack payments-prod` lists the members.

### Changing a stack

**In the web interface:** each member has a **Remove** button. **Edit
details** changes the environment, owner and description, and has **Delete
stack**. Deleting a stack does not change its hosts.

**On the command line:**

```sh
hearsay stack -name payments-prod -remove lb01
hearsay stack -name payments-prod -owner platform-team
```

Flags you leave out keep their current values.

### Importing stacks from a document

If your inventory already has a stack column, the importer creates the stacks
for you. Write a mapping file that says which column means what:

```json
{
  "host_column": "Host",
  "stack_column": "Stack",
  "address_column": "Address",
  "attrs": {
    "OS": "os.version",
    "Application": "app.name",
    "Role": "role"
  }
}
```

Then import the document:

```sh
hearsay import -name inventory.md -map mapping.json inventory.md
```

The `-name` is the source name shown beside every claim from this file.
Import the same file again after you edit it; unchanged claims are not
duplicated, and stack details you set by hand are kept. A CSV file works the
same way.

## 6. Tutorial: choose what to collect

hearsay always reads the operating system, its version, the kernel, the
hardware model and the serial number. Anything else, you switch on. A
**collector** reads one value from each host.

### Step 1: pick from the catalog

The catalog has ready-made collectors for CPU cores, memory size, time zone,
DNS servers, SELinux mode, the Splunk forwarder and Splunk Enterprise
versions, Splunk's install location, and the versions of Java, Python,
OpenSSL, nginx, Apache httpd and Docker. It also has fill-in-the-blank ones:
**file exists**, **file contents**, **an environment variable**
such as `SPLUNK_HOME`, and **an installed package's version**.

**In the web interface:** open **Collectors**. Find what you want, open
**Switch on**, choose **All hosts** or only some stacks, and select **Switch
on**. Then choose a host in the **Switched on** list and select **Test** to
see what it reads. A test saves nothing.

**On the command line:**

```sh
hearsay collectors -catalog
hearsay collectors -try memory.total -host web01
hearsay collectors -enable memory.total
hearsay collectors -enable splunk.uf.version -stacks splunk-prod
hearsay collectors -enable file.exists -param /etc/resolv.conf
```

### Step 2: collect

Select **Collect now** on the **Overview** page, or run `hearsay refresh`.
The new values appear on each host's page. To see one for every host, open
**Inventory**, then **Columns**, and tick it.

### Step 3 (only if nothing fits): write your own

Any read-only command can become a collector. You write the command, a rule
for which part of its output to keep, and a name, in a file on the hearsay
server. You do not need to write a program.

```sh
hearsay collectors -init                                  # create the file
hearsay collectors -try example.nodename -host web01      # test, saves nothing
```

The **[collector guide](collectors.md)** explains the catalog in more detail
and walks through writing your own, with examples and a table of what to do
when something does not work.

## 7. Everyday use

### The daily loop

**In the web interface:** **Overview**, then **Collect now**. Then open
**Disagreements** and decide each one with **Machine wins**, **Document
wins** or **Accept**.

**On the command line:**

```sh
hearsay refresh            # collect, check and compare
hearsay disagreements      # what needs a decision
hearsay disagreements -decide 12 -as machine -note "rebuilt in May"
```

The `-as` values are `machine`, `document` and `accept`. You can settle
several at once: `-decide 12,13,14`.

### Finding hosts

On the **Inventory** page, type in the search box, filter by stack, choose
columns, and download the view as CSV. On the command line:

```sh
hearsay hosts                          # every host
hearsay hosts pay                      # names that contain "pay"
hearsay hosts -disagrees               # hosts with a disputed value
hearsay hosts -cols os.version,env,owner
hearsay hosts -json                    # for scripts
```

### Retiring a host

When a machine is gone, retire it. Its history is kept and collection stops.

**In the web interface:** on the host's page, **Retire**, with a reason. To
find it again, use **Retired hosts** on the Inventory page; its page has
**Restore host**.

**On the command line:**

```sh
hearsay host -retire web01 -reason "decommissioned, CHG-2210"
hearsay host -unretire web01
```

### Backing up

```sh
hearsay export -out backup.json
hearsay restore -db new.db -in backup.json
```

`restore` needs an empty database.

## 8. The web interface

| Page | What it shows | What you can change |
|---|---|---|
| Overview | Coverage counts and the newest disagreements | Start a collection |
| Disagreements | One card for each disagreement | Decide each one |
| Inventory | One row for each host | Add a host; choose columns; download CSV |
| Host page | Values, sources, reachability, identity, addresses | Connection, stacks, health checks, stated values, retire and restore, chosen address |
| Stacks | One card for each stack | Create, edit and delete stacks; add and remove members |
| Charts | One chart for each attribute; each bar links to its hosts | |
| Runs | The collection history | |
| Collectors | Every collector that will run, and the catalog | Switch catalog collectors on and off, choose their stacks, test one on a host |
| Guide | This document | |

Every change is written to the audit log with the name of the person who
made it. After a change, a message at the top of the page says what
happened, or why it was not saved.

Every filter is in the URL. Copy the address bar to share exactly what you
see.

### Security

The web interface has no login of its own. Either keep it on `127.0.0.1`, or
put it behind a proxy that authenticates people and passes their name in the
`X-Forwarded-User` header; hearsay records that name.

Forms only accept submissions from the hearsay page itself, so another web
site cannot make changes through your browser. If a proxy serves hearsay
under a different name than the one it forwards, list that name when
starting:

```sh
hearsay serve -trusted-origin https://cmdb.example.com
```

## 9. Troubleshooting

**"no agent is listening"**: start ssh-agent and add your key. On Windows,
start the "OpenSSH Authentication Agent" service.

**"matches 2 items"**: two hosts share that name. Use the fqdn or the id that
the error lists.

**A value shows "not understood"**: the host answered, but no rule read the
answer. Look at the raw output with `hearsay evidence -host <name> -show`,
then fix the parse rule (`reparse`) or the normaliser rule (`renormalize`).

**My collector did not run**: run `hearsay collectors` and check that it is
listed, then test it with `hearsay collectors -try <id> -host <name>`. The
[collector guide](collectors.md#when-it-does-not-work) lists the common causes.

**A host shows "never" collected**: it has no enabled connection. Add one on
the host's page, under **Connection**.

**"Not saved: this connection uses a credential the web interface cannot
change"**: the connection uses a password or a helper program. Change it with
`hearsay target`.

**A form answers "403 Forbidden"**: the browser reached hearsay under a
different name than the proxy forwards. Start it with
`-trusted-origin https://<the name in the address bar>`.

**A path is refused as "C:/Program Files/Git/..."**: Git Bash on Windows
rewrites arguments that start with `/`. Run `export MSYS_NO_PATHCONV=1`
first, or use PowerShell or the web page.

**Nothing in the disagreements list after a collect**: run `hearsay detect`,
or use `hearsay refresh`, which runs it for you.

## 10. Glossary

| Term | Meaning |
|---|---|
| Assertion | One claim by one source about one attribute of one host |
| Attribute | The name of a value, such as `os.version`. You can make up new ones |
| CI | Configuration item. A host is one kind |
| Collector | Something that reads a value from a host |
| Connection profile | How to reach a host: transport, address, user, credential reference |
| Disagreement | A finding that needs a person to decide |
| Evidence | The stored output of a collector command |
| Normaliser | A rule that turns a raw string into a comparable value |
| Scope | Separates several instances of the same thing on one host |
| Stack | A named group of hosts that make up one deployed system |
