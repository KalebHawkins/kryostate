# The API

This document is for persons who write scripts and tools that read the
KryoState inventory. KryoState serves all the data of its pages as JSON
under `/api/v1`. You can use the API for a CMDB sync, a monitoring check, a
ticket hook, a spreadsheet or a list of hosts for a script.

The API is **read-only**. To change hosts, stacks or rules, use the web
interface or the `kryostate` command.

- `GET /api/v1` shows all endpoints.
- `GET /api/v1/openapi.json` gives the OpenAPI 3.1 description of all
  endpoints. You can make a client from it, or import it into Postman or
  Swagger UI.

## Try it

```sh
curl -s http://localhost:8099/api/v1
curl -s http://localhost:8099/api/v1/status
curl -s 'http://localhost:8099/api/v1/hosts?fields=os.version,application'
```

## Access

`kryostate serve` serves the API on the same address as the web pages. The
API has no login. A person who can open the pages can also read the API. By
default, the server listens only on `127.0.0.1`.

**WARNING:** If the server listens on a network address, all persons on that
network can read the inventory. Before you change the address, put a proxy
that asks for a login in front of it, or use a firewall.

The API never gives secrets:

- A connection shows only the transport, address, port, user and if it is
  enabled.
- The API does not give credential references, host keys, notification
  destinations or passwords.

## Conventions

| Item | Rule |
|---|---|
| Methods | `GET` and `HEAD` only. Other methods get `405` with `Allow: GET, HEAD` |
| Format | JSON, `Content-Type: application/json; charset=utf-8`, `Cache-Control: no-store` |
| Times | RFC 3339 in UTC, for example `2026-11-12T08:00:00Z` |
| Hosts in a URL | The id, the first six or more characters of the id, or a name of the host: `/api/v1/hosts/web01` |
| Lists | An object with `items`, `total`, `limit`, `offset`, and `next` if there are more pages |
| Paging | `limit` (1 to 5000, default 500) and `offset` (0 or more) |
| Errors | `{"error": {"status": 400, "message": "query: ..."}}`, with the same HTTP status |
| Unknown endpoint | `404` with a JSON error |
| Missing values | `null` if you asked for the attribute with `fields`. If not, the key is not there |

A list has this shape:

```json
{"items": [ ... ], "total": 1200, "limit": 500, "offset": 0,
 "next": "/api/v1/hosts?limit=500&offset=500"}
```

A host, a stack, a rule and a change each have a `url`. This is its page in
the web interface, so that a tool can link to it.

## Endpoints

| Endpoint | Parameters | Response |
|---|---|---|
| `/api/v1` | none | The list of endpoints, the version, and the links to this guide and the OpenAPI description |
| `/api/v1/openapi.json` | none | The OpenAPI 3.1 description |
| `/api/v1/status` | none | The health counts of the hosts (refer to [Status](#status)) |
| `/api/v1/hosts` | `q`, `fields`, `stack`, `health`, `unknown`, `attr`, `value`, `disabled`, `limit`, `offset` | The hosts and their values. Each host has `disabled` |
| `/api/v1/hosts/{host}` | `at` | One host: each value with its source and time, `unknown`, stacks, identities, reachability, connections (each with `enabled`), and `disabled`. A disabled host also has `disabled_at`, `disabled_by` and `disabled_reason` |
| `/api/v1/hosts/{host}/changes` | `since`, `attr`, `limit`, `offset` | The changes on one host |
| `/api/v1/stacks` | `limit`, `offset` | The stacks with the member counts and health |
| `/api/v1/stacks/{stack}` | none | One stack and its members, each with its role and values |
| `/api/v1/attributes` | `limit`, `offset` | Each recorded attribute, with the number of hosts that have it |
| `/api/v1/attributes/{attr}` | none | The number of hosts for each value, and the hosts with no value and the cause. Each bucket has `hosts_url`, which lists its hosts |
| `/api/v1/changes` | `since`, `host`, `attr`, `limit`, `offset` | The changed values, newest first |
| `/api/v1/rules` | `limit`, `offset` | The rollout rules with their done, not yet and unknown counts |
| `/api/v1/rules/{rule}` | none | One rule, by id or name: the hosts in each bucket with the values that put them there, and the history of the rule |
| `/api/v1/collectors` | `limit`, `offset` | The collectors: what each collects, on which hosts, its schedule and its last run |
| `/api/v1/runs` | `limit`, `offset` | The recent collection runs |

### Host parameters

| Parameter | Function |
|---|---|
| `q` | A query in the language of the Inventory page, or part of a name. Refer to [Queries](queries.md). You must URL-encode it |
| `fields` | Attributes to return in `values`, separated by commas. A host with no value gets `null`. Without `fields`, each host has all its values |
| `stack` | Only the members of this stack |
| `health` | Only the hosts in one health bucket: `failing`, `stale`, `never` or `noconn` |
| `unknown` | Only the hosts with no value for this attribute |
| `attr`, `value` | Only the hosts where the attribute `attr` has the value `value` |
| `disabled` | `true`: only the disabled hosts |
| `at` | For `/api/v1/hosts/{host}` only. The values at the end of this day (`2026-03-01`) or at this time (`2026-03-01T15:04`). The response then has `as_of` and no `unknown` |

```sh
curl -s -G http://localhost:8099/api/v1/hosts \
  --data-urlencode 'q=stack == "web" && nginx.version < "1.24"' \
  --data-urlencode 'fields=nginx.version'
```

With `fields`, all items have the same keys.

### Since

`since` accepts one of these values. The default is seven days.

| Type | Example |
|---|---|
| A date | `2026-11-01` |
| A time | `2026-11-01T08:00:00Z` |
| An age | `24h` or `7d` |

### Status

The counts are of enabled hosts, except `disabled`.

| Field | Contents |
|---|---|
| `hosts` | The number of enabled hosts |
| `ok` | The hosts where the last collection was fully or partly correct |
| `failing` | The hosts where the last collection failed |
| `stale` | The hosts with no good collection in the last `stale_after_hours` hours |
| `never_collected` | The hosts that were never collected |
| `no_connection` | The hosts with no enabled target |
| `disabled` | The disabled hosts. They are not in the other counts |
| `stale_after_hours` | The age, in hours, after which a host is stale |
| `healthy` | `true` if `failing`, `stale`, `never_collected` and `no_connection` are all `0` |
| `last_run` | The last collection run, if there is one |

### Unknown values

The `unknown` list of a host has the values that the host could not give at
the last try, with the error and the start time of the problem. These values
are not in `facts` or `values`, because the old value is possibly not
correct now.

## Recipes

### Every host to CSV

```sh
curl -s 'http://localhost:8099/api/v1/hosts?limit=5000&fields=os.version,application' |
  jq -r '.items[] | [.name, .values["os.version"], .values.application, (.stacks | map(.stack) | join(";"))] | @csv'
```

The **CSV** link on the Inventory page gives the same result without `jq`.

### A monitoring check

This command exits with a non-zero status if a host failed, is stale, was
never collected or has no enabled target:

```sh
curl -sf http://localhost:8099/api/v1/status | jq -e '.healthy' >/dev/null
```

### The hosts that a rollout waits for

```sh
curl -s 'http://localhost:8099/api/v1/rules/nginx%201.24' |
  jq -r '.not_yet[] | "\(.name)\t\(.values["nginx.version"])"'
```

### Changes in the last day

```sh
curl -s 'http://localhost:8099/api/v1/changes?since=24h' |
  jq -r '.items[] | "\(.host): \(.attr) \(.old) -> \(.new)"'
```

### All pages, in Python

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

`kryostate host list --json` gives a list of hosts. A script can change this
list into an Ansible inventory. On a different host, get the same list from
`/api/v1/hosts?q=...`. Make the groups from `stacks[].stack` and `role`.

## Stability

Version `v1` will not remove a field or change the meaning of a field.
New endpoints and new fields can be added. Thus a client must ignore the
fields that it does not know. A change that is not compatible will be in
`/api/v2`, next to `v1`.
