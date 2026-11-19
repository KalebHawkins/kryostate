# KryoState user guide

This guide is for persons who are new to KryoState. It gives the basic
concepts, then shows how to add a host, make a stack and select the values to
collect. The web interface shows this guide at `/guide`. The
[query guide](queries.md) is the reference for queries. The
[collector guide](collectors.md) shows how to write your own collectors.
`docs/operations.md` gives all settings, `docs/adr/` gives the design
decisions and `docs/threat-model.md` gives the security assumptions.

## 1. What KryoState does

KryoState is an inventory of hosts. It connects to each host and reads the
values from the host. You give it only the data that a host cannot report:
which hosts exist, the stacks of each host, the role of the host in each
stack, and the application of the host.

## 2. Basic concepts

### 2.1 Values and sources

Each value is an assertion with five parts: host (`web01`), attribute
(`os.version`), value (`RHEL 8.6`), source (for example the `ssh` collector)
and time. KryoState does not overwrite a value. It keeps the old value and
adds the new value, so the database keeps the history of each host.

### 2.2 The host always wins

A value from a collector on the host wins over all other sources. If no
collector gave a value, the source with the lowest rank number wins.

| Source | Rank | Where the value comes from |
|---|---|---|
| collector | first | A command that runs on the host |
| human | 20 | A person, with `kryostate host update` |
| dns | 30 | A DNS lookup |
| document | 40 | A document that an older version imported (application only) |
| derived | 90 | A value calculated from a different value |

A person can set only the **application** of a host, its **stacks** and its
**role** in each stack. All other values come from the host.

### 2.3 Host IDs

Each host has a random ID, because a name can be used again for a different
host. In a command, identify a host by its name, its fqdn, or the first six
characters of its ID. If a name matches two hosts, the command stops and
shows the two hosts.

### 2.4 Unknown is not blank

If a host has no value for an attribute, KryoState puts it in one of four
groups:

| Group | Meaning | Action |
|---|---|---|
| never collected | KryoState did not try to read the host | Add a target |
| collection failed | The last try failed | Repair the access |
| not understood | The host replied, but no rule could read the output | Repair or add a rule |
| host disabled | KryoState does not contact the host | No action, or enable the host |

If a command ran on the host and failed, the old value becomes unknown. For
example, the user cannot read a folder. Queries then use no value, rules put
the host in **Unknown**, and `kryostate host show` shows the error. The next
good value replaces the unknown value. If the connection stops during a
command, the values do not change.

## 3. Installation

KryoState is one binary file that contains the web interface.

| Procedure | Steps |
|---|---|
| Release archive | Download the archive for Linux, macOS or Windows from the GitHub releases page. Put `kryostate` on your `PATH`. |
| `.deb` or `.rpm` package | Install the package. It adds a `kryostate` system user, the settings file `/etc/kryostate/kryostate.env` and the database folder `/var/lib/kryostate`. Run `sudo systemctl enable --now kryostate`. Refer to `docs/operations.md`. |
| Container image | Run the command below. |
| Go 1.27.4 or later | Run the command below. |

```sh
podman run -d --name kryostate -p 127.0.0.1:8099:8099 -v kryostate-data:/data \
  ghcr.io/kalebhawkins/kryostate:latest
go install github.com/KalebHawkins/kryostate/cmd/kryostate@latest
```

`kryostate version` shows the version. The **Guide** page also shows it at
the bottom.

The database is one SQLite file in your user configuration folder. To use a
different file, set `KRYOSTATE_DB` or give `--db`. You can put settings in
`kryostate/config.yaml` in the same folder, with the flag names as keys (for
example `db:`). A flag has priority over a `KRYOSTATE_` environment variable,
and a variable has priority over the file.

Each command is a noun and a verb, for example `kryostate host add`. Run
`kryostate --help` for all commands, and `kryostate <command> --help` for the
flags. KryoState writes your name in the audit log. It gets the name from
`KRYOSTATE_ACTOR`, then `USER`, then `USERNAME`.

### Trying it without your own servers

The repository has a Podman lab with KryoState, an ssh-agent and four Linux
hosts. With Docker, use `docker` instead of `podman`.

1. Run `deploy/lab/lab.sh up`.
2. Open `http://localhost:8099`.
3. Do the tutorials with the hosts `web01`, `web02`, `db01` and `app01`, and
   the user `kryostate`.

To add all lab hosts and collect one time, run `deploy/lab/lab.sh seed`. To
run a command in the lab, use `podman exec -i kryostate-lab kryostate <command>`.

## 4. Tutorial: add your first host

This tutorial adds one Linux host, collects from it and shows the result.
Replace `web01.example.com` and `svc-ro` with your host and a read-only
account.

### Step 0: start the web interface

**WARNING:** The web interface has no login. A person who can get access to
the port can change the inventory. Keep the address on `127.0.0.1`, or use a
proxy that does the login (section 8).

1. Run `kryostate serve --addr 127.0.0.1:8099`.
2. Open `http://127.0.0.1:8099` in a browser.

### Step 1: add the host

1. Open **Inventory**.
2. Open **Add a host**.
3. Enter the name and, if you know it, the fqdn.
4. Select **Add host**.

On the command line, run `kryostate host add web01 --fqdn web01.example.com`.
Give the fqdn if you know it, because a short name can be the same at two
sites.

### Step 2: add a target

A target tells KryoState how to log in to a host: transport, address, user
and credential. You add and change targets on the command line only. The
**Connection** card on the host page shows the targets, but you cannot change
them there.

```sh
kryostate target add web01 --user svc-ro
```

| Flag | Use |
|---|---|
| `--user`, `-u` | The user name. |
| `--credential`, `-c` | A credential reference: `agent:`, `keyring:service/account`, `file:/path/key`, `exec:/path/helper` or `env:VAR`. |
| `--password` | Ask for a password and keep it in the credential store of the operating system. |
| `--address`, `-a` | The address. The default is the fqdn, then the name. |
| `--transport`, `-t` | `ssh` (default) or `winrm`. WinRM uses HTTPS only. |
| `--port`, `-p` | The port. Zero uses the default of the transport. |
| `--bastion` | A bastion host, as `user@host:port/credential-ref`. |
| `--disable` | Keep the target, but do not collect through it. |

The database keeps only the credential reference, not the key or password.
`kryostate target update web01 --port 2222` changes only the flags that you
give. `kryostate target list` shows all targets. `kryostate target rm web01`
removes the targets of a host, and the values stay.

If you do not give `--user`, `target add` uses the user of your other
targets. If they use more than one user, you must give `--user`. If you do
not give `--credential`, it uses the credential of your other targets for
that user. If there is none, it uses a stored password, then `agent:`.

#### Logging in with a password

If you cannot put a key on each host, use a password:

```sh
kryostate target add web01 web02 db01 --user svc-ro --password
```

The command asks for the password two times. It keeps the password as
`kryostate/svc-ro` in Windows Credential Manager, the macOS keychain or the
Secret Service. Then it sets the targets to `keyring:kryostate/svc-ro`. If a
host asks for the password at a keyboard-interactive prompt, KryoState
replies there.

To change the credential of all targets of one user, run one of these:

```sh
kryostate target set-credential --for-user svc-ro --password
kryostate target set-credential --for-user svc-ro --credential agent:
```

If the password changes, run `kryostate secret set kryostate/svc-ro`. All
targets that use the password read this one entry.

Give `secret set` the part of the reference after `keyring:`. It refuses a
whole reference such as `keyring:kryostate/svc-ro`, because no target reads
an entry stored under that name.

#### Find and remove stored passwords

1. Run `kryostate secret list`. It shows each stored password, the number of
   connections that use it, entries made by mistake, and references that are
   missing.
2. Run each command that the list shows under **To fix**.

To remove an entry by the name in the **STORED AS** column, run
`kryostate secret delete --entry '<name>'`. On macOS and Linux the credential
store cannot be listed, so the list shows only the keyring references in use.

#### Lockout protection

**CAUTION:** A directory account usually locks after some incorrect
passwords. If KryoState tries an old password on many hosts, the account can
lock. KryoState gives this protection for each password credential
(`keyring:`, `env:`, `exec:`, and all WinRM targets):

- In a run, the hosts try one at a time until the password works. Then they
  connect in parallel.
- Each connection sends the password one time only.
- A WinRM target tests the password with one command before the collectors.
- After two authentication failures in sequence, the credential is **on
  hold**. The count continues across hosts, runs and restarts. A good login
  sets the count to zero.
- KryoState does not try a credential that is on hold. Its hosts show "not
  tried: … is on hold".

| Command | Result |
|---|---|
| `kryostate secret holds` | Shows the credentials that failed, and which are on hold. |
| `kryostate target list` | Marks the targets of a held credential `(on hold)`. |
| `kryostate secret set kryostate/svc-ro` | Stores a new password and releases the hold. `--password` also releases it. |
| `kryostate secret release keyring:kryostate/svc-ro` | Releases the hold. Use it only if the password is correct. |

### Step 3: collect

1. Open **Runs**.
2. Select **Collect now**.

On the command line, run `kryostate refresh --only web01`. If you do not give
`--only`, `refresh` collects from all hosts. Both procedures run the built-in
read-only commands and all collectors that you switched on. `refresh` also
runs the health checks (step 6). If the connection fails, KryoState shows the
cause, for example no ssh-agent or a changed host key (section 9).

### Step 4: read the result

Open the host page. The **Facts** table shows each value, its source and its
time. On the command line, run `kryostate host show web01`.

### Step 5: set the application

A host cannot report its application, so you set it:

```sh
kryostate host update web01 application=payments
kryostate host update web01 stack=payments-prod role=app --reason "per CHG-1042"
```

KryoState refuses all other attributes, for example `env=production`.
`kryostate set` is a short name for `kryostate host update`. `--reason` adds a
note to the audit log entry. In the web interface, you can set the
application only when you add hosts to a stack (section 5).

### Step 6 (optional): add a health check

A health check tells you if a service replies. It is independent of the
login. You add health checks on the command line only:

```sh
kryostate check add web01 https --http https://web01.example.com/health --expect 2xx
kryostate check add db01 postgres --tcp db01.example.com:5432
kryostate check list                 # all checks, with the host and last result
kryostate check list web01
kryostate check rm web01 https
kryostate check                      # run all enabled checks
```

`kryostate refresh` also runs the checks. **Collect now** and the schedules
do not run them.

## 5. Tutorial: build a stack

A **stack** is a deployed system, for example "payments in production". A
host can be in more than one stack, with a **role** in each stack, for
example `app`. KryoState does not find stacks. You tell it which hosts are in
a stack.

### Add a list of hosts

You can make a stack, its hosts, their roles and their targets in one step.
Write one host on each line: the name, then the role.

```text
web01 app
web02 app
db01.example.com database
lb01 loadbalancer lb01.example.com
```

A name with dots is the fqdn, first or last on the line. Spaces, tabs or
commas can separate the words, so you can copy columns from a spreadsheet.

1. Open **Stacks**.
2. Open **New stack**.
3. Enter a name, for example `payments-prod`, and optionally the
   environment, owner and description.
4. In **Hosts**, enter the list.
5. Optional: in **Connect new hosts as user**, enter a user. Each new host
   gets a target with that user and your ssh-agent.
6. Optional: in **Application**, enter the application of the listed hosts.
7. Select **Create stack**.

To add more hosts later, see the next section.

On the command line, put the list in a file and run:

```sh
kryostate stack add payments-prod --environment production \
    --owner payments-team --file hosts.txt --create --user svc-ro --application payments
```

`--create` adds unknown hosts. Without it, KryoState shows each unknown name
and does not add it. `--credential` sets the credential of new targets (the
default is `agent:`). For a short list, give the hosts after the stack name:
`kryostate stack add payments-prod web01:app db01:database`. If the stack
does not exist, `stack add` makes it.

### Add more hosts to a stack

Open the stack from the **Stacks** list, then use one of these controls:

| Control | Use |
|---|---|
| **Host** and **Add host** | Type a part of a host name, select the host, and optionally give a **Role**. |
| **Add hosts by query** | Enter a query. The page shows how many hosts it adds. The button adds them. |
| **Add several hosts by name** | Enter a list, as for **New stack**. |

```sh
kryostate stack add payments-prod web01 --role app
kryostate stack add payments-prod -q 'os.family == "Linux" && name in ("web01", "web02")' --role app
```

Hosts that are already in the stack keep their role. A query adds the hosts
one time. It does not add hosts that match the query later.

### Look at and change a stack

The **Stacks** page shows each stack with its counts: collected, failed and
never collected. The page of a stack shows each member and its role. **Open
in inventory** shows the members with their values. On the stack page:

- **Remove** removes a member.
- **Edit details** changes the environment, the owner and the description.
- **Delete stack** deletes the stack. The hosts do not change.

```sh
kryostate stack list
kryostate host list --stack payments-prod
kryostate stack rm payments-prod lb01               # remove a member
kryostate stack update payments-prod --owner platform-team
kryostate stack delete payments-prod                # the hosts stay
kryostate collectors enable group.web --stacks payments-prod
```

`stack update` changes only the flags that you give. `collectors enable ...
--stacks` adds the stack to where the collector runs. Other hosts of the
collector do not change.

### Importing hosts in bulk

You can add many hosts, with their targets and stacks, from one YAML or JSON
file:

```yaml
defaults:            # optional; the target fields of a host override these
  transport: ssh
  user: ops
  credential: keyring:kryostate/ops
  port: 22
stacks:              # optional stack details
  - name: payments-prod
    description: Payment services
    owner: team-payments
    environment: prod
hosts:
  - name: web01
    fqdn: web01.example.com
    application: ledger
    stacks: [payments-prod:app, {stack: edge, role: proxy}]
    target: {user: svc-ro, address: 192.0.2.10, bastion: "ops@jump.example.com:22/agent:"}
  - name: db01
    stacks: [payments-prod:database]
  - name: lab01
    target: false    # in the inventory, with no target
```

**CAUTION:** In YAML, `agent:` without quotes is not a value. Write
`credential: "agent:"`.

1. Run `kryostate import --example > hosts.yaml` to get a file with comments.
2. Edit the file.
3. Run `kryostate import hosts.yaml --dry-run` to see the changes.
4. Run `kryostate import hosts.yaml`.

`kryostate import - --format json < hosts.json` reads standard input.
`kryostate import --from-db > hosts.yaml` writes the current hosts, targets
and stacks in the same format.

- KryoState checks all of the file first. If there is a problem, for example
  an unknown key, it shows the line and writes nothing.
- KryoState finds a recorded host by its name or fqdn. If you import the same
  file again, nothing changes. A pinned host key does not change.
- KryoState removes nothing. Hosts, targets and stack members that are not in
  the file do not change.
- A credential is always a reference, never a password. If a `keyring:`
  credential is not stored, a warning gives the `kryostate secret set`
  command.
- The application in the file has the source "human", so a value from the
  host wins.

## 6. Tutorial: choose what to collect

KryoState always reads the operating system, its version, the kernel, the
hardware model and the serial number. For other values, switch on a
**collector**. A collector reads one value from each host.

### Step 1: select from the catalog

The catalog has collectors for CPU cores, memory size, time zone, DNS
servers and SELinux mode. It also has collectors for the versions of Java,
Python, OpenSSL, nginx, Apache httpd and Docker. The collectors in the table
below need a parameter:

| Collector | Parameter |
|---|---|
| `file.exists`, `file.contents` | A file path |
| `config.value` | A configuration file, a key and optionally a section, for example `max_connections` in `postgresql.conf` |
| `env.var` | An environment variable, for example `JAVA_HOME` |
| `package.version` | An installed package |

A **group** switches on more than one collector: **Host basics** (CPU,
memory, time zone, DNS), **Language runtimes** and **Web servers**. The group
buttons are above the list on the **Collectors** page.

**CAUTION:** `file.contents`, `config.value` and `env.var` can show secrets.
The web interface switches them on only if `kryostate serve` has
`--allow-content-collectors`. If not, use the command line.

1. Open **Collectors**.
2. Find the collector with the search box or the filters, and select it.
3. Under **Where it runs**, open **Switch on**.
4. If the collector needs a parameter, enter it.
5. In **Runs on**, enter a query that selects the hosts, or leave it empty
   for all hosts.
6. Select **Switch on**.

Examples for **Runs on** are `stack == "web"` and
`name in ("web01", "web02")`. A preview shows how many hosts the query
selects. If a collector has commands for more than one system, it runs the
correct command on each host. You do not need a query for the system.

The collector page then shows where it runs, for example **Hosts in web**.
**Change where it runs** changes the query. To test the collector, type a
host name under **Test on a host**, select the host, then select **Test**. A
test does not save the value. The page offers only the hosts that the
collector runs on. If the host's system has no command, the test runs nothing
and tells you so. To test a content collector on the page, start the server
with `serve --allow-content-collectors`. `kryostate collectors try` has none
of these limits.

```sh
kryostate collectors catalog
kryostate collectors try memory.total --host web01
kryostate collectors enable memory.total
kryostate collectors enable group.web --stacks web
kryostate collectors enable nginx.version --where 'name in ("web01", "web02")'
kryostate collectors enable file.exists --param /etc/ssh/sshd_config.d/hardening.conf
kryostate collectors enable config.value --param /etc/app/app.conf \
    --section database --key host
```

The host page shows the values with readable names, for example **nginx
version**. KryoState groups a version by its first two numbers, so the charts
show 1.22.1 and 1.22.4 together. The **Raw** column and `kryostate host show`
show the full version.

### Step 2: collect

A new collector runs at its scheduled time. To collect before that time,
select **Collect now** on the **Runs** page, or run `kryostate refresh`. To
show a value for all hosts, open **Inventory**, open **Columns** and select
the value.

### Step 3 (only if no catalog collector fits): write your own

You can make a collector from a read-only command. In a JSON file, you write
the command, a rule that selects the part of the output to keep, and a name.
To start, copy a collector that exists: on its page, open **View definition**
and select **Copy**.

```sh
kryostate collectors add --example > mine.json          # an example file
kryostate collectors show cpu.cores                     # an example to copy
kryostate collectors add mine.json --dry-run            # check, save nothing
kryostate collectors add mine.json
kryostate collectors try example.kernel --host web01    # test, save nothing
```

KryoState keeps the added collectors in the database. A running server uses
them without a restart. To replace a collector, add it again with the same
ID. `kryostate collectors rm <id>` removes it, and its values stay. The
[collector guide](collectors.md) gives the full procedure and a list of
problems and solutions.

## 7. Everyday use

### The daily loop

While `kryostate serve` runs, KryoState collects on a schedule. The
**Overview** shows if all hosts collected, which hosts fail and why, and the
changes of the last day. To collect from all hosts now, select **Collect
now** on the **Runs** page, or run `kryostate refresh`.

Only one collection runs on a database at a time. If a different process
collects, `refresh` and `collect` show who started it, then wait up to 10
minutes. `--wait 30m` changes the time. `--no-wait` stops at once. While a
`refresh` runs, the schedule waits and **Collect now** does not start.

### Schedules

While `kryostate serve` runs, each collector runs on its own schedule.
Hardware, versions and the operating system run one time each day. Settings
and file checks run every 6 hours. Scheduled runs have these limits, to keep
the load on the hosts low:

- A scheduled run connects only to the hosts of its collector.
- A maximum of 4 sessions are open at the same time, and 1 on each host.
- The hosts start at random times in a period of up to 2 minutes.
- If a host cannot be reached, the schedule stops for 10 minutes for that
  host. After each failure the time doubles, to a maximum of 6 hours. A
  restart does not change it. The host page shows the wait.
- **Collect now** and `refresh` still try the host. One good reply stops the
  wait.
- No scheduled run starts during **Collect now**.
- The shortest interval is 5 minutes.

If a schedule has a time of day (`--at 02:00`), the collector runs at that
time. If not, KryoState calculates a fixed time from the name of the
collector, so that the collectors do not all start in the same minute. A
collector does not run again less than one hour after its last run (half the
interval, if the interval is shorter than two hours).

The **Schedules** page shows the runs of the next 24 hours, and the
collectors in sequence by their next time. If a group of collectors needs too
many sessions, it shows **Busy slot**. **Spread these over 2h** gives each
daily collector in the group a different time. To change a schedule:

1. On the row of the collector, select **Edit**.
2. Select an interval, **Manual only**, or **Custom** (for example `90m`).
3. For one day or more, select a time, keep **Picked for it**, or select
   **Use a quiet time**.
4. Select **Save**.

```sh
kryostate collectors schedules
kryostate collectors schedule builtin.os --every 1d --at 02:00
kryostate collectors schedule cpu.cores --every 6h
kryostate collectors schedule cpu.cores --manual              # or --default
```

The built-in collectors are `builtin.os`, `builtin.hw` and `builtin.host`.
Your own collectors are `spec.<id>`, and external programs are
`external.<name>`. A running server uses a change in less than one minute. To
collect only when a person starts it, start the server with `--no-schedule`.

### Changes

A change is a collected value that is different from the last value from the
same collector. The **Changes** page shows the changes of the last 24 hours,
7 days or 30 days. On the host page, **changed** marks a value that changed
in the last week. Put the pointer on it to see the old value.

On the command line, `kryostate changes` shows the same list:

```sh
kryostate changes                           # the last 24 hours
kryostate changes web01 --since 30d         # one host
kryostate changes --attr os.version --since 7d
kryostate changes --json
```

`--since` takes hours (`24h`), days (`7d`) or a duration such as `90m`.

### Notifications

KryoState can send a daily digest of changes to a chat channel or by email.
The digest shows the changed values, with a maximum of 20 hosts and 10 values
for each host. It also shows the hosts that started to fail or recovered, and
the hosts that were never collected. If no value changed and no host started
to fail or recovered, KryoState sends no digest. **Send even when nothing
happened** changes this.

You add destinations on the command line only. The **Notifications** page has
a link on the **Changes** page. It shows the destinations, the recent sends
and a **Preview**. On it, you can switch the digest on or off, set the time,
select the sections and use **Send a test now**.

**WARNING:** A webhook URL contains its token. KryoState does not accept a
webhook URL or an SMTP password as a value, so the database does not keep
them. Give a credential reference: `env:NAME`, `file:/path`, `exec:…` or
`keyring:service/account`.

```sh
export KRYOSTATE_SLACK_URL='https://hooks.slack.com/services/…'
kryostate notify add-webhook ops --url-ref env:KRYOSTATE_SLACK_URL     # Slack or classic Teams
kryostate notify add-webhook feed --url-ref file:/etc/kryostate/feed-url --format generic
kryostate notify add-email team --to ops@example.com,dba@example.com \
  --from kryostate@example.com --smtp smtp.example.com:587 \
  --user kryostate --password-ref env:KRYOSTATE_SMTP_PASSWORD --starttls   # or --tls for port 465
kryostate notify set --base-url https://kryostate.example.com   # the link in each digest
kryostate notify set --at 08:00 --enable                        # --disable stops it
kryostate notify list
kryostate notify test ops
kryostate notify preview
kryostate notify rm ops
```

The process that sends reads the reference: the server for the digest, the
command for `notify test`. Thus you must set an `env:` reference in the
environment of the server. KryoState sends a password only on an encrypted
connection. The `generic` format sends a JSON object, for example for a Teams
workflow.

KryoState sends the digest one time each day. If the server was stopped at
that time, it sends when it starts that day. If a send fails, KryoState tries
three times, then shows the error. A server with `--no-schedule` sends no
digest.

### Rules: tracking a rollout

A rule follows a change on many hosts. Before the change, you write two
queries. The **scope** selects the hosts. The **expect** query gives the
value that each host must report after the change. When the hosts are
collected again, they move from **Not yet** to **Done**. A rule does not
change a host, and you do not mark hosts as done.

| List | Meaning |
|---|---|
| **Done** | The host matches the expect query. |
| **Not yet** | The values do not match yet, for example nginx 1.20 when 1.24 is expected. |
| **Unknown** | The host did not report a value that the expect query needs (see 2.4). The list shows the missing value. |

A rule is **complete** when all hosts in the scope are done. If the scope
matches no host, the rule is never complete.

#### Worked example: roll out the SSH hardening drop-in

All Linux hosts in the web stack must have the file
`/etc/ssh/sshd_config.d/hardening.conf`.

1. On **Collectors**, switch on **File exists** with the path
   `/etc/ssh/sshd_config.d/hardening.conf`.
2. On **Rules**, open **New rule**.
3. Enter the name `SSH hardening drop-in`.
4. Enter the scope `stack == "web" && os.family == "Linux"`.
5. Enter the expect query
   `file.exists["/etc/ssh/sshd_config.d/hardening.conf"] == true`.
6. Save the rule.

A preview shows how many hosts the scope covers. KryoState does not save a
query that has an error. After the next collection, the hosts with the file
are **Done**. Install the file on the other hosts. The **Not yet** list has a
link to those hosts in the inventory.

The **Overview** shows the open rules under **Rollouts**. The page of a rule
shows 50 hosts for each list, with their values, and a trend chart. If you
change the queries of a rule, the chart starts again.

#### Track a rollout on the command line

1. Make the rule:

   ```sh
   kryostate rules add 'nginx 1.24' --scope 'stack == "web"' --expect 'nginx.version >= "1.24"' \
       --owner web-team --description 'CHG-1234, by June'
   ```

2. Run `kryostate rules list`. It shows each rule with the number of hosts
   that are done, not yet done and unknown.
3. Run `kryostate rules show 'nginx 1.24'`. It shows the hosts that are not
   yet done, and the unknown hosts with the value that is missing.
4. After you change a host, run `kryostate refresh --only <host>`. The host
   moves to **Done** when its new value matches.
5. When the rollout is complete, run `kryostate rules rm 'nginx 1.24'`. This
   deletes the rule and its history. It does not change a host.

For a report of all hosts in the scope, run
`kryostate rules show 'nginx 1.24' --json`. To find the attribute names for
the expect query, run `kryostate host show <host>`. If you omit `--scope`,
the rule covers all hosts.

A disabled host stays in the scope of a rule. To leave out disabled hosts,
add `&& !disabled` to the scope, for example
`stack == "web" && !disabled`.

### A host on a past day

To see the values of a host on a past day, select the date in **As of**
above the **Facts** table, then select **Show**. On the command line:

```sh
kryostate host show web01 --at 2026-03-01          # at the end of 1 March
kryostate host show web01 --at 2026-03-01T15:04    # at that minute
```

The other parts of the host page show the present.

### Finding hosts

On the **Inventory** page, type a part of a name or a query in the box. Use
**Columns** to select the columns. You can download the result as CSV or
JSON. The URL contains the query (`/hosts?q=...`), so you can bookmark it or
send it to a different person.

```sh
kryostate host list                      # all hosts
kryostate host list pay                  # names that contain "pay"
kryostate host list -q 'os.family == "Linux" && stack == "web"'
kryostate host list --cols os.version,application
kryostate host list --json               # for scripts
```

#### Queries

A query is a condition on the current values of each host, for example
`stack == "data" && role == "database"` or `!has(os.version)`.

| Operators | Use |
|---|---|
| `==` `!=` | Compare values |
| `=~` `!~` | Match a pattern |
| `<` `<=` `>` `>=` | Compare numbers and versions |
| `in` | Find a value in a list |
| `&&` `\|\|` `!` `( )` | Join conditions |

The names are attributes, for example `os.version`, and also `name`, `id`,
`stack`, `role` and `application`. Text with no query symbols, for example
`web`, finds host names. A collector uses the same queries to select its
hosts (section 6). The [query guide](queries.md) is the full reference, also
at **Guide → Queries** (`/guide/queries`).

#### JSON export

The read-only API at `/api/v1` gives all data that the pages show. Refer to
the [API guide](api.md), also at **Guide → API** (`/guide/api`).

`/hosts.json` accepts the same `q` and `col` parameters as the **Inventory**
page. `kryostate host list --json` writes the same structure:

```json
[{"id": "01J9...", "name": "web01", "stacks": [{"stack": "web", "role": "web"}],
  "application": "nginx", "values": {"os.version": "RHEL 8.6", "env": null},
  "last_collected": "2026-11-12T08:00:00Z", "collect_ok": true}]
```

`values` has the selected columns, with `null` if the host has no value.
`col=*` selects all attributes. The command line gives all values, unless you
give `--cols`. If there is no `application` or `last_collected`, the field is
not there.

### Disabling and enabling a host

Disable a host that KryoState must not contact for a period, for example
during maintenance. KryoState does not collect from a disabled host, and does
not run external collectors, health checks or DNS lookups on it. The host
stays in the inventory, its stacks, the charts and the queries, marked
**Disabled**. It is not in the counts of failed or never-collected hosts.
Its targets do not change, so they operate as before when you enable the
host.

1. Open the host page.
2. Select **Disable** next to the name.
3. Confirm.

The page then shows **Disabled — not contacted**, with who disabled it and when. It also shows the reason, if `host disable --reason` gave one. To
enable the host, select **Enable**. **Disabled hosts** on the **Inventory**
page shows all disabled hosts.

```sh
kryostate host disable web01 web02 --reason "powered off, CHG-2210"
kryostate host enable web01
kryostate host list -q 'disabled == true'
```

`target list` shows **HOST DISABLED** for the targets of a disabled host.
`refresh --only web01` on a disabled host contacts nothing. To stop one
target only, run `kryostate target update web01 --disable`.

### Deleting a host

**WARNING:** `host rm` deletes the host and all its history. You cannot undo
it. To stop contact with a host and keep its data, use `host disable`.

```sh
kryostate host rm web01 --reason "decommissioned"   # asks first; --yes does not ask
```

The command removes all values, stored output, probes, identities,
addresses, checks, stack memberships and targets of the host. The audit log
keeps its entries, and records the name, the person and the reason.

### Backing up

```sh
kryostate export --out backup.json
kryostate restore --db new.db --in backup.json
```

`restore` needs an empty database.

## 8. The web interface

| Page | What it shows | What you can change |
|---|---|---|
| Overview | If all hosts collected; the hosts that need attention; the changes of the last day; the next runs; hosts by operating system, stack and application | Nothing |
| Inventory | One row for each host, with a query filter and the status | Add a host; select columns; show disabled hosts; download CSV or JSON |
| Host page | Values, sources, collectors, targets (read-only), reachability, identities and addresses | Disable or enable the host; select the address if DNS gives more than one; show a past day |
| Stacks | Each stack, with its members, roles and counts | Create, edit and delete stacks; add and remove members; set the application of added hosts |
| Charts | One chart for each attribute, with links to the hosts of each value | Nothing |
| Changes | Collected values that changed | Nothing |
| Rules | Each rule, with its progress and trend | Create, edit and delete rules |
| Runs | The collection history | Start a collection |
| Schedules | The next run of each collector, and busy slots | Change a schedule; spread a busy slot |
| Collectors | All collectors, with a search box, filters and the groups | Switch on a group |
| Collector page | One collector: what it records, where it runs, its commands, its **Schedule**, and **View definition** | Switch a catalog collector on or off; select its hosts; test it; change its schedule |
| Notifications | The digest settings, the destinations, a preview and the recent sends | Change the settings; send a test |
| Guide | This guide, the collector guide, the query guide and the API guide | Nothing |

You change targets, health checks, notification destinations and your own
collectors on the command line only. KryoState writes each change in the
audit log with the name of the person. The URL contains all filters, so you
can copy it to share a view.

### Security

**WARNING:** The web interface has no login. A person who can get access to
it can change the inventory. Keep it on `127.0.0.1`, or use a proxy that does
the login and sends the user name in the `X-Forwarded-User` header.

The forms accept data only from the KryoState pages, so a different web site
cannot make changes through your browser. If a proxy serves KryoState with a
different name, start the server with that name:

```sh
kryostate serve --trusted-origin https://cmdb.example.com
```

## 9. Troubleshooting

| Problem | Action |
|---|---|
| "no agent is listening" | Start ssh-agent and add your key. On Windows, start the "OpenSSH Authentication Agent" service. |
| "host key does not match the recorded key" | Do the procedure below. |
| "matches 2 items" | Two hosts have that name. Use the fqdn or the ID that the error shows. |
| A value shows "not understood" | Run `kryostate evidence list --host <name> --show` to see the output. Repair the parse rule and run `kryostate reparse`, or repair the normaliser rule and run `kryostate renormalize`. |
| A collector did not run | Run `kryostate collectors list` and `kryostate collectors try <id> --host <name>`. Refer to the [collector guide](collectors.md#when-it-does-not-work). |
| A host shows "never" collected | The host has no enabled target. Run `kryostate target add <host> --user <user>`. |
| "a scheduled run in another kryostate serve ... has been running since" | A different process collects. Wait, or run the command again later with a longer `--wait`. If a process stops during a run, the lock becomes free after a maximum of 2 minutes. |
| A host is "skipped by the schedule until ..." | Repair the cause in the message. Then run `kryostate refresh --only <host>`. |
| "not tried: … is on hold" | Refer to Lockout protection in section 4. |
| A form shows "403 Forbidden" | Start the server with `--trusted-origin https://<the name in the address bar>`. |
| A path changes to "C:/Program Files/Git/..." | Git Bash changes arguments that start with `/`. Run `export MSYS_NO_PATHCONV=1`, or use PowerShell. |

### A changed host key

**WARNING:** A changed host key can show that a different system uses the
address of the host. Compare the fingerprints before you clear the key.

1. On the host, run `ssh-keygen -lf /etc/ssh/ssh_host_ecdsa_key.pub`.
2. Compare the fingerprint with the fingerprint in the message.
3. If the fingerprints are different, stop and find the cause.
4. If the fingerprints are the same, run `kryostate target forget-host-key web01`.
5. Collect from the host. The next collection pins the new key.

### A name that gives several addresses

DNS can give more than one address for a host, for example the host's own
address and a virtual IP that moves between hosts.

**CAUTION:** If KryoState connects through a virtual IP, the host key changes
when the IP moves to a different host. Then the connection is refused. Choose
the host's own address.

1. Run `kryostate resolve` to record the addresses that DNS gives.
2. Run `kryostate host address web01` to show the addresses.
3. Run `kryostate host address web01 192.0.2.11` to choose one.

You can also choose the address on the host page. All targets of the host use
the chosen address. To use the address of the target again, run
`kryostate host address web01 --clear`.

## 10. Glossary

| Term | Meaning |
|---|---|
| Assertion | One value from one source for one attribute of one host |
| Attribute | The name of a value, for example `os.version` |
| Change | A collected value that is different from the last value from the same collector |
| Collector | A command or program that reads a value from a host |
| Evidence | The stored output of a collector command |
| External program | A program that runs on the KryoState server one time for each host. It gets the host in `KRYOSTATE_TARGET` and `KRYOSTATE_ADDRESS`, and writes a JSON envelope of facts. You list it in `externals.json` next to the database |
| Normaliser | A rule that changes a raw string into a value that KryoState can compare |
| Query | A condition, for example `os.family == "Linux"`, that selects hosts |
| Rule | A scope query and an expect query that follow a change on many hosts |
| Stack | A named group of hosts that make one deployed system |
| Target | How KryoState connects to a host: transport, address, user and credential reference |
