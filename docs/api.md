# The API

Everything the KryoState pages show can also be fetched as JSON from
`/api/v1`. It is for other tools, such as:

- a CMDB sync
- a monitoring check
- a ticketing hook
- a spreadsheet refresh
- a script that needs a list of hosts

The API is **read-only**. Hosts, stacks and rules are changed in the web
interface or with the `kryostate` command.

- `GET /api/v1` lists every endpoint.
- `GET /api/v1/openapi.json` describes them all in OpenAPI 3.1. Most
  languages can generate a client from it, and tools such as Postman or
  Swagger UI can import it.

## Try it

```sh
curl -s http://localhost:8099/api/v1
curl -s http://localhost:8099/api/v1/status
curl -s 'http://localhost:8099/api/v1/hosts?fields=os.version,application'
```

## Access

The API is served by `kryostate serve` on the same address as the web pages,
and is open to whoever can open those pages. There is no login yet, for the
pages or the API. By default the server listens only on `127.0.0.1`. Before
listening on a network address, put it behind a reverse proxy that
authenticates, or restrict it with a firewall to the machines that need it.

Nothing secret is ever returned:

- Connections show the user, address and port.
- Credential references, host keys, notification destinations and
  passwords are left out.

## Conventions

| | |
|---|---|
| Format | JSON, `Content-Type: application/json` |
| Times | RFC 3339 in UTC, such as `2026-11-12T08:00:00Z` |
| Hosts | Named in a URL by id, an id prefix of six or more characters, or any name the host is known by: `/api/v1/hosts/web01` |
| Lists | An object with `items`, `total`, `limit`, `offset` and, while more pages remain, `next` (below) |
| Paging | `limit` (1 to 5000, default 500) and `offset` |
| Errors | `{"error": {"status": 400, "message": "query: ..."}}` with the same HTTP status |
| Missing | A value a host does not have is `null` when you asked for it with `fields`, and absent otherwise |

A list looks like this:

```json
{"items": [ ... ], "total": 1200, "limit": 500, "offset": 0,
 "next": "/api/v1/hosts?limit=500&offset=500"}
```

One host, and every stack, rule and change, carries a `url`: its page in
the web interface, so a tool can link back.

## Endpoints

| Endpoint | What it returns |
|---|---|
| `/api/v1/status` | How many hosts are collected, failing, stale, never collected or have no connection, and `healthy` |
| `/api/v1/hosts` | Hosts and their values. Takes `q`, `fields`, `stack`, `health`, `unknown`, `retired` |
| `/api/v1/hosts/{host}` | One host: every value with its source and when it was seen, what is unknown and why, stacks, identities, reachability, connections. `?at=2026-03-01` gives the values as they were that day |
| `/api/v1/hosts/{host}/changes` | What changed on one host. Takes `since` and `attr` |
| `/api/v1/stacks` | Stacks with member counts and health |
| `/api/v1/stacks/{stack}` | One stack and its members, each with their role and values |
| `/api/v1/attributes` | Every attribute recorded, with how many hosts have it |
| `/api/v1/attributes/{attr}` | How many hosts hold each value, and how many have none and why. Each bucket's `hosts_url` lists its hosts |
| `/api/v1/changes` | Values that changed, newest first. Takes `since`, `host` and `attr` |
| `/api/v1/rules` | Rollout rules with done, not yet and unknown counts |
| `/api/v1/rules/{rule}` | One rule, by id or name: the hosts in each bucket with the values that put them there, and its history |
| `/api/v1/collectors` | What is collected, on which hosts, on what schedule, and when it last ran |
| `/api/v1/runs` | Recent collection runs |

### Choosing hosts

`/api/v1/hosts` takes the same query language as the Inventory page's
search box (see [Queries](/guide/queries)), in `q`. Remember to URL-encode
it:

```sh
curl -s -G http://localhost:8099/api/v1/hosts \
  --data-urlencode 'q=stack == "web" && nginx.version < "1.24"' \
  --data-urlencode 'fields=nginx.version'
```

Without `fields`, each host carries every value it has. With it, only those
attributes are returned, with `null` where a host has none, so every item
has the same keys.

### Since

`since` takes:

- a date: `2026-11-01`
- a time: `2026-11-01T08:00:00Z`
- an age: `24h` or `7d`

It defaults to seven days.

### Unknown values

A host's `unknown` list holds the values it could not report on its last
try, with the error and since when. Those values are left out of `facts`
and `values`, because the old answer is no longer known to be true.

## Recipes

### Every host to CSV

```sh
curl -s 'http://localhost:8099/api/v1/hosts?limit=5000&fields=os.version,application' |
  jq -r '.items[] | [.name, .values["os.version"], .values.application, (.stacks | map(.stack) | join(";"))] | @csv'
```

The Inventory page's **CSV** link gives the same thing without `jq`.

### A monitoring check

Exits non-zero when any host failed, went stale or was never collected:

```sh
curl -sf http://localhost:8099/api/v1/status | jq -e '.healthy' >/dev/null
```

### Which hosts a rollout is waiting on

```sh
curl -s 'http://localhost:8099/api/v1/rules/nginx%201.24' |
  jq -r '.not_yet[] | "\(.name)\t\(.values["nginx.version"])"'
```

### Changes since yesterday, for a ticket or a chat message

```sh
curl -s 'http://localhost:8099/api/v1/changes?since=24h' |
  jq -r '.items[] | "\(.host): \(.attr) \(.old) -> \(.new)"'
```

### Every page, in Python

```python
import requests

base = "http://localhost:8099"
url = "/api/v1/hosts?q=" + requests.utils.quote('os.family == "Linux"')
hosts = []
while url:
    page = requests.get(base + url, timeout=30).json()
    hosts += page["items"]
    url = page.get("next")
print(len(hosts), "hosts")
```

### PowerShell

```powershell
$r = Invoke-RestMethod 'http://localhost:8099/api/v1/hosts?fields=os.version'
$r.items | Select-Object name, @{n='os'; e={$_.values.'os.version'}}
```

### Ansible

`kryostate hosts -json` already prints an inventory a script can turn into
Ansible's format. From another machine, the same list comes from
`/api/v1/hosts?q=...`. Group the hosts by `stacks[].stack` and `role`.

## Stability

`v1` will not remove a field or change what one means. New endpoints and
new fields may be added, so a client should ignore fields it does not know.
Anything incompatible will appear as `/api/v2` beside `v1`.
