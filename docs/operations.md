# Running KryoState

This document is for the person who runs KryoState for other users. It tells
how to run KryoState as a service on Linux or in a container, and how to back
it up, upgrade it and configure it. To install KryoState, refer to the
[README](../README.md#install). To use it, refer to the
[user guide](user-guide.md).

## Where things are

The `.deb` and `.rpm` packages install these files:

| Path | Contents |
|---|---|
| `/usr/bin/kryostate` | The program. It contains the web interface |
| `/usr/lib/systemd/system/kryostate.service` | The service unit (from `deploy/systemd`) |
| `/usr/lib/systemd/system/kryostate-ssh-agent.service` | An ssh-agent for the service. You can use it or not |
| `/etc/kryostate/kryostate.env` | The settings of the service. An upgrade keeps your copy |
| `/var/lib/kryostate/kryostate.db` | The database: all hosts, values, changes and audit entries |
| `/var/lib/kryostate/collectors.json`, `rules.json`, `externals.json` | Your collectors, normaliser rules and external programs, if you add them |

The package makes a system user and group with the name `kryostate`. The
service runs as this user. The service can write only to `/var/lib/kryostate`.

## Running as a service

1. Start the service and enable it at boot:

   ```sh
   sudo systemctl enable --now kryostate
   ```

2. Make sure that the service runs:

   ```sh
   systemctl status kryostate
   journalctl -u kryostate -f
   ```

The service listens on `127.0.0.1:8099`.

**WARNING:** The web interface and the JSON API have no login. A person who
can open them can change the inventory. Keep the service on `127.0.0.1`, and
put a proxy that asks for a login in front of it.

### Change the settings of the service

The file `/etc/kryostate/kryostate.env` has `KEY=value` lines that systemd
reads. `KRYOSTATE_OPTS` holds all other `kryostate serve` flags.

1. Edit `/etc/kryostate/kryostate.env`. For example, if the proxy uses a
   different name, add that origin:

   ```sh
   KRYOSTATE_OPTS=--trusted-origin https://cmdb.example.com
   ```

2. Restart the service:

   ```sh
   sudo systemctl restart kryostate
   ```

For a change to the unit itself, use `systemctl edit kryostate`. Then a
package upgrade does not remove your change.

### Use the command line with the service database

Run the command as the `kryostate` user. Then the service can read the files
that the command makes.

```sh
sudo -u kryostate kryostate host list --db /var/lib/kryostate/kryostate.db
sudo -u kryostate KRYOSTATE_DB=/var/lib/kryostate/kryostate.db kryostate refresh
```

SQLite lets only one process write at a time. One collection runs on a
database at a time. A `refresh` waits for a scheduled collection to finish
(`--wait`, 10 minutes by default). `--no-wait` makes it stop with an error
at once.

## SSH access for the service user

The service collects as the `kryostate` user. This user has no login session,
and its home directory is `/var/lib/kryostate`. The credential reference of
each target tells KryoState how to log in (refer to
[Credentials](../README.md#credentials)). The service user needs different
things for each type of reference.

### `agent:`

Use this type if possible, because KryoState never reads a private key. The
unit hides `/home` and `/run/user`, so the service cannot use the ssh-agent
of a person. Use the `kryostate-ssh-agent` service:

1. Start the agent:

   ```sh
   sudo systemctl enable --now kryostate-ssh-agent
   ```

2. Make a directory for the key:

   ```sh
   sudo install -d -o kryostate -g kryostate -m 0700 /etc/kryostate/ssh
   ```

3. Copy the key into the directory:

   ```sh
   sudo install -o kryostate -g kryostate -m 0600 id_ed25519 /etc/kryostate/ssh/
   ```

4. Add the key to the agent. If the key has a passphrase, `ssh-add` asks for
   it:

   ```sh
   sudo -u kryostate SSH_AUTH_SOCK=/run/kryostate-agent/agent.sock ssh-add /etc/kryostate/ssh/id_ed25519
   ```

`kryostate.env` sets `SSH_AUTH_SOCK` to the socket of this agent. The agent
keeps the key in memory only. After a reboot or an agent restart, do step 4
again. If you keep a copy of the key in a different place, you can delete
the key file after step 4.

### `file:`

For `file:/etc/kryostate/ssh/id_ed25519.age`, an age-encrypted key:

1. Set `KRYOSTATE_AGE_IDENTITY=/etc/kryostate/age-identity.txt` in
   `kryostate.env`.
2. Make the key file and the identity file owned by `kryostate`, with mode
   `0600`.

With this type, the service can collect after a reboot without a person.
But the identity is on the same host as the key. A key file without
encryption also works, but it is less safe.

### `exec:`

The helper program runs as `kryostate` with the same limits as the service.
The file system is read-only, except `/var/lib/kryostate`. There are no home
directories, `/tmp` is private, and the program cannot get more privileges.
Thus `sudo` does not work in the helper.

### `keyring:`

A service cannot use `keyring:`. On Linux, it needs the Secret Service of a
desktop session, and a system user has no desktop session. Use a different
type.

### `env:`

Put the variable in `kryostate.env`. This file has mode `0640`, is owned by
`root`, and the `kryostate` group can read it.

### Host keys

KryoState pins the key of each host in its database at the first connection.
After that, it refuses a changed key. It does not read or write
`known_hosts`. After a host is rebuilt, run
`kryostate target forget-host-key <host>`.

**CAUTION:** A database backup that contains keys gives the means to log in
to your hosts. Keep keys and identities in `/etc/kryostate`, not in
`/var/lib/kryostate`.

## Container

The image has no shell. It runs as an unprivileged user (uid 65532). The
database is `/data/kryostate.db`. Put `collectors.json`, `rules.json` and
`externals.json` in the `/data` volume. The released images are
`ghcr.io/kalebhawkins/kryostate:<version>` and `:latest`.

```sh
podman run -d --name kryostate -p 127.0.0.1:8099:8099 -v kryostate-data:/data \
  ghcr.io/kalebhawkins/kryostate:latest
podman exec kryostate kryostate host list
```

To build an image from the source, use `deploy/Containerfile`:

```sh
podman build -t kryostate -f deploy/Containerfile .
```

To collect, the container needs an ssh-agent. An ssh-agent answers only its
own user, so an agent of a different user does not work. For one solution,
refer to `deploy/lab/agent.Containerfile`. It runs an agent in its own
container as uid 65532, and shares only the socket through a volume.

## Backups

The database is one SQLite file. You can back it up in two ways.

To make a JSON export while the service runs:

```sh
sudo -u kryostate kryostate export --db /var/lib/kryostate/kryostate.db \
  --out /var/lib/kryostate/backup-$(date +%F).kryostate.json
```

To copy the file:

1. Stop the service: `sudo systemctl stop kryostate`.
2. Copy the file: `sudo cp -a /var/lib/kryostate/kryostate.db /backup/`.
3. Start the service: `sudo systemctl start kryostate`.

**CAUTION:** An export contains host names, addresses and credential
references. It does not contain credentials. Keep it in a safe place.

The export does not contain `collectors.json`, `rules.json` or
`externals.json`. Back up these files separately.

In a container, make the export in the container. Then copy it out:

```sh
podman exec kryostate kryostate export --out /data/backup.kryostate.json
podman cp kryostate:/data/backup.kryostate.json .
```

### Restore

`restore` loads an export only into a new, empty database.

1. Load the export:

   ```sh
   sudo -u kryostate kryostate restore --db /var/lib/kryostate/restored.db --in backup.kryostate.json
   ```

2. In `kryostate.env`, set `KRYOSTATE_DB=/var/lib/kryostate/restored.db`.
3. Restart the service: `sudo systemctl restart kryostate`.

## Upgrades

1. Make an export (refer to [Backups](#backups)).
2. Install the new version:
   - Package: `sudo apt install ./kryostate_*.deb` or
     `sudo dnf install ./kryostate-*.rpm`. The package restarts a running
     service. It keeps `/etc/kryostate/kryostate.env` and the database.
   - Archive: replace the binary, then restart the program.
   - Container: pull the new tag, then make the container again with the
     same volume.
3. Make sure that the new version runs: `kryostate version`. The Guide page
   of the web interface also shows the version at the bottom.

### Migrations

The new version updates the database when it first opens it. You do not
have to run a separate migration command. Keep the export from step 1. To go
back to the earlier version, use the earlier binary and restore that export
into an empty database.

## Remove the package

The package removal stops and disables the services. It does not remove the
database, the settings or the `kryostate` user.

**WARNING:** The commands below delete the inventory and its history
permanently. Make an export first.

```sh
sudo rm -r /var/lib/kryostate /etc/kryostate
sudo userdel kryostate
```

## Configuration

KryoState reads each setting from the first of these sources that has it:

1. A flag on the command line, for example `--concurrency 16`.
2. An environment variable: `KRYOSTATE_` and the flag name in capitals, with
   `_` for `-`. For example, `KRYOSTATE_HOST_TIMEOUT=2m`.
3. The config file.
4. The default value.

Only the settings in the table below come from the environment or the config
file. Each setting applies to the commands that have that flag.

| Setting | Environment variable | Commands | Default |
|---|---|---|---|
| `db` | `KRYOSTATE_DB` | all | `kryostate/kryostate.db` in the user configuration directory |
| `addr` | `KRYOSTATE_ADDR` | `serve` | `127.0.0.1:8099` |
| `concurrency` | `KRYOSTATE_CONCURRENCY` | `collect`, `refresh` | `8` |
| `host-timeout` | `KRYOSTATE_HOST_TIMEOUT` | `collect`, `refresh` | `2m` |
| `rules` | `KRYOSTATE_RULES` | `collect`, `refresh`, `serve`, `reparse`, `renormalize` | `rules.json` next to the database, if it exists |
| `collectors` | `KRYOSTATE_COLLECTORS` | `collect`, `refresh`, `serve`, `reparse`, `collectors` | `collectors.json` next to the database, if it exists |
| `externals` | `KRYOSTATE_EXTERNALS` | `collect`, `refresh`, `serve` | `externals.json` next to the database, if it exists |
| `no-evidence` | `KRYOSTATE_NO_EVIDENCE` | `collect`, `refresh`, `serve` | `false` |
| `stale-after` | `KRYOSTATE_STALE_AFTER` | `serve` | `336h`. Used only if the built-in collectors run by hand |
| `trusted-origin` | `KRYOSTATE_TRUSTED_ORIGIN` | `serve` | none |
| `allow-content-collectors` | `KRYOSTATE_ALLOW_CONTENT_COLLECTORS` | `serve` | `false` |
| `no-schedule` | `KRYOSTATE_NO_SCHEDULE` | `serve` | `false` |
| `wait` | `KRYOSTATE_WAIT` | `collect`, `refresh` | `10m` |
| `no-wait` | `KRYOSTATE_NO_WAIT` | `collect`, `refresh` | `false` |

### Config file

The config file is YAML. Its default location is `kryostate/config.yaml` in
the user configuration directory. On Linux, this is
`$XDG_CONFIG_HOME/kryostate/config.yaml`, usually
`~/.config/kryostate/config.yaml`. `--config` or `KRYOSTATE_CONFIG` gives a
different file.

If the default file does not exist, KryoState continues without it. If the
file from `--config` or `KRYOSTATE_CONFIG` does not exist, the command stops
with an error.

The keys are flag names. A key at the top level applies to all commands. A
key under the name of a command applies only to that command. For a
subcommand, use one level for each word, for example `host:` then `add:`.

```yaml
db: /var/lib/kryostate/kryostate.db
concurrency: 16
serve:
  addr: 0.0.0.0:8099
  trusted-origin: https://cmdb.example.com
```

The service reads its settings from `kryostate.env`. To give the service a
config file, set `KRYOSTATE_CONFIG` in `kryostate.env`, for example to
`/etc/kryostate/config.yaml`.

### Other environment variables

| Variable | Function |
|---|---|
| `KRYOSTATE_CONFIG` | The config file |
| `KRYOSTATE_ACTOR` | Your name in the audit log. If it is not set, KryoState uses `USER`, then `USERNAME` |
| `KRYOSTATE_AGE_IDENTITY` | The age identity that decrypts `file:...age` credentials |
| `SSH_AUTH_SOCK` | The ssh-agent socket for `agent:` credentials |
| `NO_COLOR` | If set, tables have no borders and no colours. `TERM=dumb` has the same result |
| `CLICOLOR_FORCE` | If set and not `0`, tables have borders and colours also in a pipe |

## JSON API

`kryostate serve` also serves a read-only JSON API under `/api/v1`, on the
same address as the web interface. The API has no login. It gives the
inventory, but never credential references, host keys or passwords. If you
make the web interface available on the network, the API is also available.

- To keep the API private, keep the listen address on `127.0.0.1`.
- To give other tools access, put a proxy that asks for a login in front of
  it, or let a firewall accept only the hosts that need it.

For the endpoints, refer to the [API guide](api.md).
