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

A health check tests if a service on the host replies. It does not log in.
You add health checks on the command line only:

```sh
kryostate check add web01 https --http https://web01.example.com/health --expect 2xx
kryostate check add db01 postgres --tcp db01.example.com:5432
kryostate check list                 # all checks, with the host and last result
kryostate check                      # run all enabled checks
```

`kryostate refresh` also runs the checks. **Collect now** and the schedules
do not run them. Refer to [Health checks](#health-checks) for all of the
options and the results.

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

Use `kryostate import` to add or change many hosts in one step. One file
gives the hosts, their targets, and their stacks with a role in each stack.
The file must be YAML or JSON.

**CAUTION:** KryoState does not import CSV or Markdown files. If your data is
in a CSV file or a spreadsheet, convert it to JSON first. Refer to
[Convert a CSV file to an import file](#convert-a-csv-file-to-an-import-file).

| Task | Command |
|---|---|
| Add or change hosts, targets and stack memberships from a file | `kryostate import hosts.yaml` |
| Write the current hosts, targets and stacks to an import file | `kryostate import --from-db > hosts.yaml` |
| Add a list of hosts to a stack | `kryostate stack add payments-prod --file hosts.txt --create` |
| Add all hosts that match a query to a stack | `kryostate stack add linux -q 'os.family == "Linux"' --role server` |
| Give the same target to many hosts | `kryostate target add web01 web02 db01 --user ops` |
| Change the credential of all targets of one user | `kryostate target set-credential --for-user ops --password` |
| Copy all of the store to a different database | `kryostate export` and `kryostate restore` |

#### Import procedure

**WARNING:** Do not put a password in the file. A file is easy to copy, and
the password can go to persons who must not have it. Give a credential
reference in the file, and store the password in the OS credential store
(step 7).

1. Run `kryostate import --example > hosts.yaml`. The command writes an example file with comments.
2. Edit `hosts.yaml`. Write one entry in `hosts` for each host. Refer to [Import file reference](#import-file-reference).
3. Run `kryostate import hosts.yaml --dry-run`. The command shows the changes and writes nothing.
4. If the command shows problems, correct the file. Then do step 3 again. Refer to [Import errors](#import-errors).
5. Make sure that the changes are correct.
6. Run `kryostate import hosts.yaml`.
7. If the output shows `warning: credential keyring:kryostate/<user> is not in the OS credential store`, run `kryostate secret set kryostate/<user>`.
8. Run `kryostate refresh` to collect from the hosts.

The output of `--dry-run` gives each change, then one line with the totals:

```text
create host web01
  fqdn web01.example.com
  application ledger
  target ssh: create svc-ro@192.0.2.10 using keyring:kryostate/ops via ops@jump.example.com:22/agent:
  stack payments-prod: add as app
  stack edge: add as proxy
...
create stack payments-prod
create stack edge
warning: credential keyring:kryostate/ops is not in the OS credential store; store it with: kryostate secret set kryostate/ops
5 hosts created, 0 updated, 0 unchanged; 4 targets created, 0 updated; 4 stack memberships added, 0 roles changed; 2 stacks created, 0 updated; 1 warning
dry run: nothing was written; nothing is removed: hosts, targets and stack memberships not in the file are left as they are
```

If you import the same file again, nothing changes. The output is
`nothing to change: the store already matches the file (5 hosts)`.

To read standard input, give `-` and `--format yaml` or `--format json`:

```sh
kryostate import - --format json < hosts.json             # sh, bash
Get-Content hosts.json | kryostate import - --format json  # PowerShell
```

#### Import file reference

The file has three top-level keys. YAML and JSON files have the same
structure. KryoState gets the format from the file extension: `.yaml`,
`.yml` or `.json`. For a different extension, a file that starts with `{` is
JSON. All other files are YAML.

| Key | Required | Content |
|---|---|---|
| `defaults` | No | Target fields for each host: `transport`, `port`, `user`, `credential` and `bastion`. You cannot put `address` or `disabled` here. |
| `stacks` | No | A list of stack details: `name` (required), `description`, `owner` and `environment`. |
| `hosts` | Yes | A list of hosts. The list must have one host or more. |

Fields of a host:

| Field | Required | Content |
|---|---|---|
| `name` | Yes | The name of the host. It cannot contain spaces or `/`. A name can occur only one time in the file. Upper and lower case are the same. |
| `fqdn` | No | The fully qualified name, for example `web01.example.com`. It must contain a dot. An fqdn can occur only one time in the file. |
| `application` | No | The application of the host. KryoState records it with the source "human". |
| `stacks` | No | The stacks of the host, with the role in each stack. Refer to the table of stack entries. |
| `target` | No | The target of the host: a mapping of target fields, `false` or `true`. Refer to the table of target rules. |
| `disabled` | No | `true` disables the host. `false` enables the host. If you do not give it, the status of the host does not change. |

Fields of a target, in the `target` of a host or in `defaults`:

| Field | Default | Content |
|---|---|---|
| `transport` | `ssh` | `ssh` or `winrm`. |
| `address` | Refer to the address order | The address or name to connect to. Only in the `target` of a host. |
| `port` | 22 for `ssh`, 5986 for `winrm` | A number from 1 to 65535. |
| `user` | None | The user to log in as. Each target must have a user, from `target` or from `defaults`. |
| `credential` | `agent:` | A credential reference: `agent:`, `keyring:service/account`, `env:NAME`, `file:PATH` or `exec:COMMAND`. Never a password. |
| `bastion` | None | A jump host, for `ssh` only: `user@host[:port]/credential-reference`. |
| `disabled` | `false` | `true` records the target, but KryoState does not collect through it. Only in the `target` of a host. |

Stack entries, in the `stacks` of a host:

| Form | Example | Result |
|---|---|---|
| `stack` | `payments-prod` | The host is a member with no role. If the host is already a member, it keeps its role. |
| `stack:role` | `payments-prod:app` | The host is a member with the role `app`. |
| `{stack, role}` | `{stack: edge, role: proxy}` | The same as `stack:role`. Use this form if the stack name contains a colon. |

Target rules:

| In the file | Result |
|---|---|
| A `target` mapping and `defaults` | Each field in the `target` of the host replaces the same field in `defaults`. The other fields come from `defaults`. |
| No `target` key, with `defaults` | The host gets a target from `defaults`. |
| No `target` key, no `defaults` | The import does not add or change a target for the host. |
| `target: true` | The host gets a target from `defaults` only. |
| `target: false` | The host gets no target. Its existing targets do not change. |

The import uses the first of these values as the address of a target:

1. The `address` in the `target` of the host.
2. The address of the existing target with the same transport.
3. The `fqdn` of the host in the file.
4. The fqdn or the hostname that KryoState has for the host.
5. The `name` of the host.

**CAUTION:** In YAML, `agent:` without quotes is not a value, because it ends
in a colon. Write `credential: "agent:"`. Also put quotes around a `bastion`
that ends in `agent:`.

**CAUTION:** A `port` in `defaults` applies also to `winrm` targets. If you
have `winrm` hosts, do not put `port` in `defaults`. Or give the `port` in
the target of each `winrm` host.

#### How the import changes recorded data

- KryoState finds a recorded host by its `name` or its `fqdn`. If the name
  and the fqdn find two different hosts, the import stops.
- If a recorded host does not have the `fqdn` of the file, the import adds
  it.
- A host has a maximum of one target for each transport. The import changes
  the target with the same transport, or adds a new target.
- A stack that only a host names gets no details. For a stack in `stacks`,
  the details that you give replace the recorded details. The other details
  do not change.
- If the host reports an application, the value from the host wins. The
  import shows a warning and does not change the application.
- The import does not change a pinned host key.
- The import removes nothing. Hosts, targets, stack memberships and roles
  that are not in the file do not change. To remove them, use
  `kryostate host rm`, `kryostate target rm` or `kryostate stack rm`.
- KryoState examines all of the file before it writes. If there is a
  problem, it shows each problem with its line number and writes nothing.
- KryoState records each change in the audit log.

**CAUTION:** The file gives all of a target, except the address and the
port. If a target field is not in the file, the import sets the default
value. For example, if the file has no `bastion` for a host, the import
removes the bastion of its target. If the file has no `disabled: true`, the
import enables a disabled target. To start from the recorded targets, use
`kryostate import --from-db`.

#### Example files

This YAML file adds five hosts, their targets and two stacks:

```yaml
defaults:                    # optional; a target field of a host replaces these
  transport: ssh
  user: ops
  credential: keyring:kryostate/ops
stacks:                      # optional stack details
  - name: payments-prod
    description: Payment services
    owner: team-payments
    environment: prod
hosts:
  - name: web01
    fqdn: web01.example.com
    application: ledger
    stacks: [payments-prod:app, {stack: edge, role: proxy}]
    target:
      user: svc-ro
      address: 192.0.2.10
      bastion: "ops@jump.example.com:22/agent:"
  - name: web02
    fqdn: web02.example.com  # also the address of the target
    stacks:
      - payments-prod:app
  - name: db01
    stacks: [payments-prod:database]
    target:
      credential: "agent:"   # quotes are necessary
  - name: win01
    target:
      transport: winrm
      user: svc-ro
  - name: lab01
    target: false            # in the inventory, with no target
    disabled: true
```

This JSON file has the same content:

```json
{
  "defaults": {
    "transport": "ssh",
    "user": "ops",
    "credential": "keyring:kryostate/ops"
  },
  "stacks": [
    {
      "name": "payments-prod",
      "description": "Payment services",
      "owner": "team-payments",
      "environment": "prod"
    }
  ],
  "hosts": [
    {
      "name": "web01",
      "fqdn": "web01.example.com",
      "application": "ledger",
      "stacks": ["payments-prod:app", {"stack": "edge", "role": "proxy"}],
      "target": {
        "user": "svc-ro",
        "address": "192.0.2.10",
        "bastion": "ops@jump.example.com:22/agent:"
      }
    },
    {
      "name": "web02",
      "fqdn": "web02.example.com",
      "stacks": ["payments-prod:app"]
    },
    {
      "name": "db01",
      "stacks": ["payments-prod:database"],
      "target": {"credential": "agent:"}
    },
    {
      "name": "win01",
      "target": {"transport": "winrm", "user": "svc-ro"}
    },
    {
      "name": "lab01",
      "target": false,
      "disabled": true
    }
  ]
}
```

### Convert a CSV file to an import file

KryoState does not import CSV. If your hosts are in a spreadsheet, save the
sheet as a CSV file. Then convert the CSV file to a JSON import file. The
examples below use a CSV file with these columns:

```text
name,fqdn,application,stacks,user,address
web01,web01.example.com,ledger,payments-prod:app;edge:proxy,svc-ro,192.0.2.10
web02,web02.example.com,ledger,payments-prod:app,,
db01,,,payments-prod:database,,192.0.2.20
lab01,,,,,
```

| Column | Content |
|---|---|
| `name` | The name of the host. Required. |
| `fqdn`, `application` | The same as the host fields. Can be empty. |
| `stacks` | Stack entries in the form `stack` or `stack:role`, separated by `;`. Can be empty. |
| `user`, `address` | Target fields of the host. Can be empty. An empty `user` comes from `defaults`. An empty `address` follows the address order. |

**CAUTION:** A JSON import file must be UTF-8 with no byte order mark. In
Windows PowerShell 5.1, `>` writes UTF-16, and `Set-Content -Encoding utf8`
adds a byte order mark. Then the import shows `not valid JSON: invalid
character`. Use `[IO.File]::WriteAllText`, as in the example below.

In PowerShell (Windows PowerShell 5.1 or PowerShell 7), run this in the
folder of `hosts.csv`:

```powershell
$hosts = Import-Csv hosts.csv | ForEach-Object {
    $h = [ordered]@{ name = $_.name }
    if ($_.fqdn)        { $h.fqdn = $_.fqdn }
    if ($_.application) { $h.application = $_.application }
    if ($_.stacks)      { $h.stacks = @($_.stacks -split ';') }
    $t = [ordered]@{}
    if ($_.user)    { $t.user = $_.user }
    if ($_.address) { $t.address = $_.address }
    if ($t.Count)   { $h.target = $t }
    $h
}
$file = [ordered]@{
    defaults = [ordered]@{ user = 'ops'; credential = 'keyring:kryostate/ops' }
    hosts    = @($hosts)
}
[IO.File]::WriteAllText("$PWD\hosts.json", ($file | ConvertTo-Json -Depth 5))
```

On Linux, `python3` does the same conversion. Save this as `csv2json.py`:

```python
import csv, json, sys
hosts = []
for r in csv.DictReader(open(sys.argv[1], newline="", encoding="utf-8-sig")):
    h = {"name": r["name"]}
    for k in ("fqdn", "application"):
        if r.get(k):
            h[k] = r[k]
    if r.get("stacks"):
        h["stacks"] = r["stacks"].split(";")
    t = {k: r[k] for k in ("user", "address") if r.get(k)}
    if t:
        h["target"] = t
    hosts.append(h)
defaults = {"user": "ops", "credential": "keyring:kryostate/ops"}
json.dump({"defaults": defaults, "hosts": hosts}, sys.stdout, indent=2)
```

Then run `python3 csv2json.py hosts.csv > hosts.json`.

After the conversion, do the [Import procedure](#import-procedure) from
step 3 with `hosts.json`. Change `defaults` in the script to your user and
credential.

### Export the inventory to an import file

`kryostate import --from-db` writes the recorded hosts, targets and stacks as
an import file. Use it to keep the inventory in a file, to make a large
change in a text editor, or to copy the hosts to a different database.

1. Run `kryostate import --from-db > hosts.yaml`.
2. Edit `hosts.yaml`.
3. Run `kryostate import hosts.yaml --dry-run`.
4. Make sure that the changes are correct.
5. Run `kryostate import hosts.yaml`.

If you import the file with no changes, nothing changes. For JSON, add
`--format json`. The file has:

- The stacks that have details, and the hosts with their `fqdn`, stacks,
  roles and `disabled` status.
- The `application` only if a person gave it. A collected application is
  not in the file.
- One target for each host. If a host has more than one target, the file
  has the `ssh` target. A note on standard error names the other targets.
- In `defaults`, the target fields that are the same for all targets.

The file does not have collected values, checks, collectors, rules, the
audit log or pinned host keys. To copy these, use `export` and `restore`.

### Other bulk commands

| Command | What it does |
|---|---|
| `kryostate stack add payments-prod --file hosts.txt --create --user svc-ro` | Adds the hosts in a list to a stack. Refer to [Add a list of hosts](#add-a-list-of-hosts). |
| `kryostate stack add payments-prod -q '<query>' --role app` | Adds all recorded hosts that match a query, one time. Hosts that match later are not added. |
| `kryostate target add web01 web02 db01 --user ops` | Gives each host the same target. Each target connects to the fqdn or the name of its host. |
| `kryostate target update web01 web02 --port 2222` | Changes the existing targets of the hosts. Only the flags that you give change. |
| `kryostate target set-credential --for-user ops --password` | Asks for the password one time, stores it as `kryostate/ops`, and gives it to all targets that log in as `ops`. |
| `kryostate target set-credential --for-user svc-ro -t winrm -c keyring:kryostate/svc-ro` | Sets a credential reference on all `winrm` targets that log in as `svc-ro`. |

In the list for `stack add --file`, write one host on each line: the name,
the role, and optionally the fqdn. Spaces, tabs, commas or semicolons can
separate the words. KryoState ignores empty lines and lines that start with
`#`. To read the list from standard input, give `--file -`.

**CAUTION:** In a host list, a word that contains a dot is the fqdn. Do not
put an IP address in the list. Give the address later with
`kryostate target update <host> --address 192.0.2.10`.

- Without `--create`, `stack add` does not add hosts that are not recorded.
  It shows each unknown name.
- `--user` gives an `ssh` target to each added host that has no `ssh`
  target. `--credential` sets its credential. The default is `agent:`.
- `target add` replaces a target with the same transport. A pinned host key
  does not change. `--address` is permitted with one host only.

### Import, export and restore

`import` and `export` do different tasks. Do not use one in place of the
other.

| | `import` and `import --from-db` | `export` and `restore` |
|---|---|---|
| Format | YAML or JSON that a person can write and edit | A JSON copy of all tables of the store |
| Content | Hosts, fqdn, application that a person gave, targets, stacks and roles | All of the store, also collected values, stored output, checks, rules, collectors, schedules, notifications, pinned host keys and the audit log |
| Database | Any database. The import adds to the recorded data and changes it. | `restore` needs an empty database. |
| Use | Load hosts in bulk. Edit and apply again. | Make a backup. Move KryoState to a different server. |

Passwords are not in either file. They stay in the OS credential store. On a
new server, store them again with `kryostate secret set kryostate/<user>`.

An `export` file is not an import file. If you give it to
`kryostate import`, the import shows `unknown key "format"` and writes nothing.

### Import errors

| Message | Cause | Action |
|---|---|---|
| `yaml: line 3: mapping values are not allowed in this context` | A value that ends in a colon, for example `agent:`, has no quotes. | Write `"agent:"`. |
| `yaml: line 2: found character that cannot start any token` | The line has a tab. | Use spaces to indent YAML. |
| `target: "password" is not accepted` | The file has a `password` key. | Remove the key. Give `credential: keyring:kryostate/<user>`. Run `kryostate secret set kryostate/<user>`. |
| `credential is not a reference (no scheme)` | The `credential` is a password or a word with no scheme. | Give a reference, for example `keyring:kryostate/ops`. |
| `host: unknown key "fqnd"` | A key is not correct. The message gives the permitted keys. | Correct the key on the line that the message gives. |
| `defaults: unknown key "address"` | `address` or `disabled` is in `defaults`. | Put it in the `target` of the host. |
| `target: no user; give user in the target or in defaults` | The target has no user. | Give `user` in the target or in `defaults`. |
| `listed twice (also on line 2)` | Two hosts have the same name or fqdn. | Remove one host. |
| `fqdn "web01" is not a fully qualified name` | The fqdn has no dot. | Give the full name, or remove `fqdn`. |
| `transport "rdp" should be ssh or winrm` | The transport is not known. | Give `ssh` or `winrm`. |
| `port should be a number from 1 to 65535` | The port is not correct. | Give a correct port. |
| `target: a bastion is for ssh only` | A `winrm` target has a bastion. | Remove `bastion`. |
| `stack payments-prod is named twice` | A host gives the same stack two times. | Remove one entry. |
| `the name matches host ... and the fqdn matches host ...` | The name and the fqdn find two different recorded hosts. | Correct the name or the fqdn in the file. |
| `matches the same recorded host as web01` | Two entries find the same recorded host. | Remove one entry. |
| `not valid JSON: invalid character '\xff'` or `'\ufeff'` | The JSON file is UTF-16 or has a byte order mark. | Write the file as UTF-8 with no byte order mark. Refer to [Convert a CSV file to an import file](#convert-a-csv-file-to-an-import-file). |
| `not valid JSON: invalid character '}' looking for beginning of object key string` | The JSON has a comma before `}` or `]`. | Remove the comma. |
| `give --format yaml or --format json when reading standard input` | The file is `-` with no format. | Add `--format yaml` or `--format json`. |
| `the file is empty` or `no hosts: the file needs a hosts: list` | The file has no `hosts` list. | Add `hosts`. Run `kryostate import --example` to see one. |
| `warning: credential keyring:kryostate/ops is not in the OS credential store` | The password is not stored. The import continues. | Run `kryostate secret set kryostate/ops`. |
| `warning: web01: the host reports application ...` | The host reports a different application. The value from the host wins. | No action is necessary. To stop the warning, remove `application` from the file. |

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
- A maximum of 4 connections are open at the same time, and 1 on each host.
  On one SSH connection, a maximum of 4 commands run at the same time. If the
  host refuses more sessions, its commands run one at a time. On WinRM,
  commands always run one at a time.
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

### Health checks

A health check tests if a service on a host replies. KryoState does not log
in for a check. It opens a TCP connection to a port, or sends an HTTP GET
request to a URL. A collection logs in and reads values. A check reads no
values. Thus a host can pass its checks and fail its collection, or the
opposite.

**CAUTION:** A check connects from the computer that runs `kryostate`, not
from the host. A firewall between the two can make a check fail while the
service is available to its users. Make sure that the KryoState computer can
reach the target.

#### Kinds of check

| | `tcp` | `http` |
|---|---|---|
| Flag | `--tcp host:port` | `--http URL` |
| Target format | `host:port`, for example `db01.example.com:5432` or `[2001:db8::5]:22`. The host and the port must not be empty. | An `http://` or `https://` URL. |
| Test | Opens a TCP connection, then closes it. Sends no data. | Sends one `GET` request with the user agent `kryostate`. Does not read the body. |
| Result `ok` | The connection opens. | The status code agrees with `--expect`. |
| `--expect` | Not used. | A status code of three characters, for example `200`, `204` or `403`. Use `x` (or `X`) for any digit after the first, for example `2xx`, `20x` or `3xx`. The default is `2xx`. |
| Redirects | Not applicable. | KryoState does not follow a redirect. A `301` or `302` is the result. Use `--expect 3xx` if a redirect is correct. |
| TLS | Not applicable. | KryoState verifies the certificate with the trusted root certificates of the computer. A certificate that is not trusted, or a different name, gives `error`. |
| Timeout | `--timeout`, default `5s`. | `--timeout`, default `5s`. The maximum is `30s`. |

Other flags of `check add`:

| Flag | Content |
|---|---|
| `--timeout` | The time limit for one check, for example `2s` or `1m`. `0s` sets the default, `5s`. |
| `--disable` | Records the check, but does not run it. To start it again, add it again without `--disable`. |

KryoState does not try a failed check again. Each run records one result.

#### Add, change and remove a check

Each check has a name that is unique on its host. You add checks on the
command line only. The web interface cannot add or change checks.

```sh
kryostate check add web01 site --http http://web01.example.com/ --expect 200
kryostate check add web01 health --http https://web01.example.com/health --timeout 10s
kryostate check add db01 postgres --tcp db01.example.com:5432
kryostate check add web02 login --http https://web02.example.com/login --expect 3xx
kryostate check rm web01 site
```

- If a check with the same name exists on the host, `check add` replaces
  all of it. Give all the flags again. Flags that you do not give get their
  default values. For example, a replaced check without `--disable` is
  enabled.
- `check rm` removes the check. It shows nothing if it is successful.
- `kryostate host rm` removes all checks of the host.

#### Run the checks

| Command | What it runs |
|---|---|
| `kryostate check` or `kryostate check run` | All enabled checks of all enabled hosts. |
| `kryostate check run --only web01,db01` | The enabled checks of the hosts that you give. |
| `kryostate refresh` | A collection, then all enabled checks of the same hosts. |
| `kryostate refresh --no-checks` | A collection only. |

`kryostate serve`, the schedules and **Collect now** do not run checks. To
run checks at regular times, run `kryostate check` from cron or the Windows
Task Scheduler.

KryoState does not run the checks of a disabled host, or a disabled check.
`check list` shows them with the status `host disabled` or `disabled`.

`check run` shows one line for each check, then the totals:

```text
HOST   CHECK     RESULT   TOOK    DETAIL
db01   postgres  ok       8ms     -
web01  health    ok       19ms    status 200
web01  site      error    23ms    http error: 503, wanted 200
2 ok, 1 failed
```

#### Results

| Result | Meaning |
|---|---|
| `ok` | The TCP connection opened, or the HTTP status agrees with `--expect`. For `http`, the detail gives the status, for example `status 200`. |
| `refused` | The host refused the connection. No service listens on the port, or a firewall refuses it. |
| `timeout` | No reply in the time limit. A firewall can drop the packets, or the service is too slow. |
| `nxdomain` | DNS does not know the name in the target. |
| `auth_failed` | The HTTP status is `401`, and `--expect` does not agree with `401`. |
| `error` | All other failures. For example, an HTTP status that does not agree with `--expect` (`http error: 503, wanted 200`), a redirect, a certificate that is not trusted, or a port that is not a number. The detail gives the cause. |

#### See the results

| Where | What it shows |
|---|---|
| `kryostate check list` | All checks of all hosts: `HOST`, `CHECK`, `KIND`, `TARGET`, `EXPECT`, `STATUS`, `LAST RESULT` and `WHEN`. A check that did not run shows `never`. |
| `kryostate check list web01` | The checks of one host. |
| `kryostate check list --json` | The same data as JSON, with the detail of the last result. |
| `kryostate host show web01` and the **Reachability** card on the host page | The last result of each kind: `tcp` for TCP checks, `health` for HTTP checks. |
| `GET /api/v1/hosts/{host}` | The same as **Reachability**, in `reachability`. |

**CAUTION:** **Reachability** shows only the last result of each kind. If a
host has two HTTP checks, it shows only the check that ran last. Use
`kryostate check list web01` to see each check.

Check results do not change the collection status of a host. The overview,
the failed hosts and the notification digest use the collection only.

#### Health check problems

| Problem | Action |
|---|---|
| `"db01" is not host:port` | Give the port, for example `--tcp db01.example.com:5432`. |
| `is not an http or https URL` | Start the URL with `http://` or `https://`. |
| `expect "20" is not a status code or a pattern like 2xx` | Give three characters. The first character must be a digit. |
| `at least one of the flags in the group [tcp http] is required` | Give `--tcp` or `--http`. Do not give both. |
| `no check named "..." on ...` | Run `kryostate check list web01` to see the names. |
| `refused` | Make sure that the service runs and listens on the port. Make sure that a firewall lets the KryoState computer connect. |
| `timeout` | Make sure that a firewall does not drop the packets. If the service is slow, give a longer `--timeout`. |
| `nxdomain` | Correct the name in the target, or use the address. |
| `error` with `http error: 301, wanted 2xx` | KryoState does not follow redirects. Give the final URL, or `--expect 3xx`. |
| `error` with `x509: certificate signed by unknown authority` | Add the root certificate to the trusted certificates of the KryoState computer. KryoState cannot skip the certificate check. |
| `auth_failed` | The URL needs a login. Use a health URL that does not need a login, or give `--expect 401`. |
| A check shows `never` | The check is disabled, the host is disabled, or no check ran after you added it. Run `kryostate check run --only web01`. |

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

`restore` needs an empty database. Passwords are not in the file. To keep
only the hosts, targets and stacks as a file that you can edit, use
`kryostate import --from-db`. Refer to
[Import, export and restore](#import-export-and-restore).

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
| `kryostate import` shows problems | Refer to [Import errors](#import-errors). |
| A health check fails | Refer to [Health check problems](#health-check-problems). |
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
