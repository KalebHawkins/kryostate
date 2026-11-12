# Running KryoState

This is for whoever runs KryoState for other people: as a service on a Linux
machine, or as a container. For installing it, see the README's Installation
section; for using it, the user guide.

## Where things are

The .deb and .rpm packages install:

| Path | What it is |
|---|---|
| `/usr/bin/kryostate` | The program. The web interface is compiled into it |
| `/usr/lib/systemd/system/kryostate.service` | The service |
| `/usr/lib/systemd/system/kryostate-ssh-agent.service` | An optional ssh-agent for the service |
| `/etc/kryostate/kryostate.env` | Settings: database path, listen address, extra flags. Kept on upgrade |
| `/var/lib/kryostate/kryostate.db` | The database: every host, value, change and audit entry |
| `/var/lib/kryostate/collectors.json`, `rules.json`, `externals.json` | Your own collectors, normaliser rules and external programs, if you add them |

The package creates a system user and group called `kryostate`. The service
runs as that user, and `/var/lib/kryostate` is the only place it can write.

In the container image the database is `/data/kryostate.db`, and the same
optional files sit beside it in the `/data` volume.

## Running as a service

```sh
sudo systemctl enable --now kryostate
systemctl status kryostate
journalctl -u kryostate -f
```

It listens on `127.0.0.1:8099`. The web interface has no login, so keep it on
localhost and put a reverse proxy that asks people to sign in in front of it.
If the proxy serves it under another name, tell KryoState so form posts from
that name are accepted:

```sh
# /etc/kryostate/kryostate.env
KRYOSTATE_OPTS=-trusted-origin https://cmdb.example.com
```

Everything in `kryostate.env` is a `KEY=value` line read by systemd;
`KRYOSTATE_OPTS` takes any `kryostate serve` flag. Restart after a change:
`sudo systemctl restart kryostate`.

While the service runs, collectors run on their schedules. To use the command
line against the same database, run it as the service user so files it
creates stay readable by the service:

```sh
sudo -u kryostate kryostate hosts -db /var/lib/kryostate/kryostate.db
sudo -u kryostate KRYOSTATE_DB=/var/lib/kryostate/kryostate.db kryostate refresh
```

SQLite allows one writer at a time; a command run while a scheduled
collection is writing waits for it.

## SSH access for the service user

The service collects as the `kryostate` user, with no login session, no home
directory of its own (its home is `/var/lib/kryostate`) and no access to
anyone else's. What each target uses to log in is its credential reference
(README, section 6). What the service user needs for each:

- **`agent:`** (preferred: no private key is ever read by KryoState). The
  service needs an agent it can reach. An agent in a person's login session
  is out of reach: the unit hides `/home` and `/run/user`, and ssh-agent only
  answers its own user anyway. The package ships one for the service:

  ```sh
  sudo systemctl enable --now kryostate-ssh-agent
  sudo install -d -o kryostate -g kryostate -m 0700 /etc/kryostate/ssh
  sudo install -o kryostate -g kryostate -m 0600 id_ed25519 /etc/kryostate/ssh/
  sudo -u kryostate SSH_AUTH_SOCK=/run/kryostate-agent/agent.sock ssh-add /etc/kryostate/ssh/id_ed25519
  ```

  `kryostate.env` already points `SSH_AUTH_SOCK` at that socket. ssh-add asks
  for the passphrase, if the key has one, and the agent keeps the key in
  memory only: after a reboot or an agent restart, run `ssh-add` again. A key
  with no passphrase can then be deleted from disk once it is loaded, if you
  keep another copy elsewhere.

- **`file:/etc/kryostate/ssh/id_ed25519.age`**. An age-encrypted key file. Set
  `KRYOSTATE_AGE_IDENTITY=/etc/kryostate/age-identity.txt` in
  `kryostate.env`, and make both files owned by `kryostate` with mode 0600.
  The service starts collecting after a reboot with nobody present, at the
  price of the identity sitting on the same machine. A plain unencrypted
  `file:` key works the same way and is weaker still.

- **`exec:/path/helper`**. The helper runs as `kryostate` under the same
  sandbox as the service: read-only file system apart from
  `/var/lib/kryostate`, no home directories, a private `/tmp`, and no way to
  gain privileges (sudo will not work).

- **`keyring:`** is not available to a service: on Linux it needs a desktop
  session's Secret Service, which a system user does not have. Use one of the
  above.

- **`env:VAR`**. Put the variable in `kryostate.env`, which is mode 0640,
  root-owned and readable by the `kryostate` group.

Host keys need nothing on disk. KryoState pins each host's key in its own
database the first time it connects and refuses a changed key after that; it
does not read or write `known_hosts`. After a host is rebuilt, clear its pin
with `kryostate target -host <host> -forget-host-key`.

Keys and identities belong under `/etc/kryostate`, never in
`/var/lib/kryostate`: a database backup should not carry the means to log in.

## Backups

The database is one SQLite file. Two ways to back it up:

```sh
# A JSON export of the whole store, safe while the service runs.
sudo -u kryostate kryostate export -db /var/lib/kryostate/kryostate.db \
  -out /var/lib/kryostate/backup-$(date +%F).kryostate.json

# Or a copy of the file, with the service stopped.
sudo systemctl stop kryostate
sudo cp -a /var/lib/kryostate/kryostate.db /backup/
sudo systemctl start kryostate
```

An export holds host names, addresses and credential *references*, never
credentials, but treat it as sensitive all the same. To restore an export,
load it into a new, empty database and point the service at it:

```sh
sudo -u kryostate kryostate restore -db /var/lib/kryostate/restored.db -in backup.kryostate.json
# then set KRYOSTATE_DB=/var/lib/kryostate/restored.db in kryostate.env and restart
```

Back up `collectors.json`, `rules.json` and `externals.json` too if you have
them; they are not in the export.

In a container, run the export inside it and copy it out:

```sh
podman exec kryostate kryostate export -out /data/backup.kryostate.json
podman cp kryostate:/data/backup.kryostate.json .
```

## Upgrades

Take an export first. Then:

- **Packages**: install the new .deb or .rpm (`sudo apt install ./kryostate_*.deb`
  or `sudo dnf install ./kryostate-*.rpm`). A running service is restarted
  onto the new version; `/etc/kryostate/kryostate.env` and the database are
  kept.
- **Archive**: replace the binary and restart whatever runs it.
- **Container**: pull the new tag and recreate the container with the same
  volume.

The database is brought up to date when the new version first opens it. Check
which version is running with `kryostate version`; the Guide page in the web
interface shows it at the bottom.

## Removing it

Removing the package stops and disables the service and leaves the database,
the settings and the `kryostate` user in place. Delete them by hand once you
have the export you want to keep:

```sh
sudo rm -r /var/lib/kryostate /etc/kryostate
sudo userdel kryostate
```
