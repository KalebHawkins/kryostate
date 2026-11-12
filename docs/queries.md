# Queries

A query picks hosts by what is known about them. It is one line, such as

```text
os.family == "Linux" && !file.exists == "/etc/ssh/sshd_config.d/hardening.conf"
```

which reads: the host runs Linux, and the SSH hardening drop-in is not there.
The style is that of a Prometheus label matcher: a name, an operator and a
value, joined with `&&` and `||`.

This page is the full reference. The short version is in the
[user guide](user-guide.md#queries).

## Where queries are used

The same language, with the same meaning, works everywhere a set of hosts is
chosen:

| Where | How |
|---|---|
| **Inventory** page | Type it into the box at the top. The query goes into the address (`/hosts?q=...`), so a filtered view can be bookmarked or sent to someone. |
| CSV and JSON downloads | The **CSV** and **JSON** links next to the box download what the box shows. `/hosts.csv?q=...` and `/hosts.json?q=...` take the query directly. |
| **Charts** | Clicking a slice or a bar opens the inventory filtered to those hosts, with the query written out so you can change it. |
| Where a collector runs | On a collector's page, **Change where it runs** takes a query. A live preview under the box counts the hosts it matches and names the first few. |
| Your own collectors | `"where"` in `collectors.json` (see the [collector guide](collectors.md)). |
| Command line | `kryostate hosts -q '...'`, `kryostate hosts -q '...' -json` and `kryostate collectors -enable <id> -where '...'`. |

So a query that lists the right hosts on the Inventory page is exactly the one
to give a collector.

## A first few

```text
os.version == "AlmaLinux 9.8"            one exact version
os.family == "Linux" && stack == "web"   Linux hosts in the web stack
name =~ "^web"                           names that start with web
name in ("web01", "db01")                two hosts, by name
hw.cpu.cores >= 8                        eight cores or more
nginx.version < "1.24"                   older nginx, compared as versions
!has(os.version)                         hosts with no OS version yet
web                                      plain text: names containing "web"
```

## Shape

Every query is built from **comparisons**. A comparison is a **name**, an
**operator** and a **value**. Comparisons are joined with `&&` (and) and `||`
(or), turned around with `!` (not), and grouped with parentheses.

```text
query       = condition { ("&&" | "||") condition }
condition   = [ "!" ] ( name [ operator value ] | name "in" list | "has(" name ")" | "(" query ")" )
name        = os.version, stack, file.exists["/etc/hosts"], ...
operator    = ==  !=  =~  !~  <  <=  >  >=
value       = "text", 'text', `text`, a number, true or false
list        = "(" value { "," value } ")", or the same in [ ]
```

`&&` binds tighter than `||`, so `a || b && c` means `a || (b && c)`. Use
parentheses when in doubt. `!` applies to the whole comparison after it:
`!os.family == "Linux"` means "not Linux".

Spaces between parts do not matter. Text with none of the query symbols in it,
such as `web` or `db01`, is not a query: it searches host names, as the box did
before there was a language.

## Names

A name is either one of the host's own fields or an attribute.

| Name | Is |
|---|---|
| `name` | the host's name |
| `id` | its internal id |
| `stack` | every stack it is a member of |
| `role` | its role in each of those stacks |
| `application` (or `app`) | the application recorded for it |
| anything else | an attribute, such as `os.version`, `hw.cpu.cores` or `nginx.version` |

To find an attribute's name:

- On a host's page, hover over a label in the Facts table. **OS Version** is
  `os.version`.
- The **Columns** list on the Inventory page names every attribute any host
  has.
- `kryostate show <host>` prints every attribute by name.
- A collector's page says what it records, under **What it records**.

### Collectors that take a path or a name

A collector that takes a value, such as **File exists** or **Package
version**, records one attribute per value, named after it. **File exists**
for `/etc/ssh/sshd_config.d/hardening.conf` records
`file.hardening_conf.exists`. You can use that name, or name the collector
with its value in brackets, which saves looking the name up:

```text
file.hardening_conf.exists == true
file.exists["/etc/ssh/sshd_config.d/hardening.conf"] == true
file.exists == "/etc/ssh/sshd_config.d/hardening.conf"
```

All three mean the same. The last is a shorthand: comparing a collector of
this kind to a string means "the collector has this value switched on and the
host reported it". For a collector that records `true` or `false` it means
`true`, and for one that records something else, such as
`package.version == "openssl"`, it means the host has a value for it.

If you gave the collector a custom name when you switched it on, the bracket
form finds that name for you.

## Operators

| Write | Means |
|---|---|
| `==` `!=` | Equal and not equal. Text is compared exactly and is case sensitive. |
| `=~` `!~` | Matches and does not match a [Go regular expression](https://pkg.go.dev/regexp/syntax). Patterns are **not anchored**: `"web"` matches anywhere in the value, `"^web"` only at the start and `"web$"` only at the end. Start a pattern with `(?i)` to ignore case. |
| `<` `<=` `>` `>=` | Order. Numbers compare as numbers (`8 < 16`). Versions compare in version order (`"7.9" < "7.10"`, `"1.0.2" < "1.0.2k"`). A value that is neither does not match. |
| `in` | The value is any one of a list: `stack in ("web", "edge")`. Same rules as `==`. Round or square brackets. |
| `has(x)` | The host has a value for `x`. |
| `x` alone | The same as `x == true`. `!x` is the same as `x != true`. |
| `&&` `\|\|` `!` `( )` | And, or, not, grouping. |

## Values

| Write | Is |
|---|---|
| `"text"` or `'text'` | A string. `\"`, `\\`, `\n` and `\t` are escapes. |
| `` `text` `` | A raw string with no escapes, which is easier for patterns full of backslashes: ``name =~ `^web\d+$` ``. |
| `8`, `-1.5` | A number. |
| `true`, `false` | Match attributes that record `true` or `false`, such as **File exists**. Older values recorded as `yes` and `no` match too. |

### Which value is compared

A query compares the **Value** column of a host's page, not the **Raw** one.
Where KryoState tidies a value up, for instance `Debian GNU/Linux 12
(bookworm)` into `Debian 12`, or a full package version into its first two
numbers, the query sees the tidy form. To match the full text, use a pattern:
`os.version =~ "bookworm"`.

## How the edge cases work

- **A missing value counts as empty text.** `os.version == ""` finds hosts
  without one, and `os.version != ""` and `has(os.version)` find hosts with
  one. A missing value never matches `<`, `>` or a pattern other than one that
  matches empty text.
- **Names with several values match when any value does.** A host in the
  stacks `web` and `edge` matches `stack == "web"` and `stack == "edge"`.
- **`!=` and `!~` are the exact opposite of `==` and `=~`.** So
  `stack != "web"` means "not in the web stack", even for a host that is also
  in another stack.
- **Ordering needs like with like.** `hw.cpu.cores > 8` compares numbers.
  `os.kernel < "5"` compares versions. `os.family < "M"` is refused when
  you write it, because "M" is neither a number nor a version, and a host whose
  value is not a number or a version never matches an ordering.
- **Unknown names are not errors.** A name no host has simply has no value, so
  `nginx.version != ""` is a quick way to find the hosts that have nginx at all.

## Mistakes

A query that does not parse is not run. The Inventory page shows the problem
with its column, and a collector's **Change where it runs** box shows a caret
under it. Common ones:

| Wrote | Problem | Write |
|---|---|---|
| `os.family = "Linux"` | `=` is not an operator | `os.family == "Linux"` |
| `os.family == Linux` | Text needs quotes | `os.family == "Linux"` |
| `name in ("web01" "web02")` | Missing comma | `name in ("web01", "web02")` |
| `file.exists == true` | Which file? | `file.exists["/etc/hosts"] == true` |
| `(stack == "web"` | Unclosed bracket | `(stack == "web")` |
| `os.family < "M"` | Order needs a number or a version | `os.family =~ "^[A-L]"` |
| `name =~ "web\d"` | `\d` is not an escape in double quotes | ``name =~ `web\d` `` |

A collector whose query does not parse runs on **no** hosts, never on all of
them. Its page says why.

## Recipes

```text
# Hosts that have never reported an OS version
!has(os.version)

# Linux hosts missing a file every Linux host should have
os.family == "Linux" && !file.exists == "/etc/ssh/sshd_config.d/hardening.conf"

# Everything in two stacks, except the databases
stack in ("web", "app") && role != "database"

# Old OpenSSL
pkg.openssl.version < "3.0"

# Big machines running a given application
application == "postgresql" && hw.memory.total_mb >= 16000

# Names that follow a pattern, ignoring case
name =~ `(?i)^(web|edge)\d+`

# One collector on a handful of named hosts only
name in ("web01", "web02", "app01")
```

## On the command line

```sh
kryostate hosts -q 'os.family == "Linux" && stack == "web"'
kryostate hosts -q 'hw.cpu.cores >= 8' -cols hw.cpu.cores,os.version
kryostate hosts -q 'stack == "data"' -json > data.json
kryostate collectors -enable nginx.version -where 'stack == "web"'
```

Put the query in single quotes so the shell leaves the double quotes and the
`!` inside it alone. On Windows PowerShell, single quotes work the same way.

## Limits

Queries run inside KryoState over the values it has loaded; nothing in them is
passed to the database, so no query can change data. They read only current
values. To see how a value changed over time, use the **Changes** page.
