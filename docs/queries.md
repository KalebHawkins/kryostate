# Queries

This page is the reference for the query language. It is for all users who
select hosts in KryoState. The [user guide](user-guide.md#queries) has a
short introduction.

A query selects hosts by their values. It is one line, such as:

```text
os.family == "Linux" && !file.exists == "/etc/ssh/sshd_config.d/hardening.conf"
```

This query selects the Linux hosts that do not have the SSH hardening file.
A query has the format of a Prometheus label matcher: a name, an operator and
a value. `&&` and `||` join the conditions.

## Where queries are used

The language is the same in all these places:

| Where | How |
|---|---|
| **Inventory** page | Type the query in the box at the top. The address contains the query (`/hosts?q=...`), so you can bookmark the filtered list or send it. |
| CSV and JSON downloads | The **CSV** and **JSON** links next to the box download the hosts that the box shows. `/hosts.csv?q=...` and `/hosts.json?q=...` take the query directly. |
| **Charts** | Select a slice or a bar to open the inventory with a query for these hosts. You can then change the query. |
| Where a collector runs | On the collector page, **Change where it runs** takes a query. The preview under the box counts the hosts that match and shows the first names. |
| Your own collectors | The `where` field of a collector (see the [collector guide](collectors.md)) |
| Stacks | **Hosts matching** on a stack page adds the hosts that match the query now. The stack does not follow the query after that. |
| **Rules** | The scope of a rule selects the hosts of a rollout. The expect query tells what each host must report when it is done, such as `nginx.version >= "1.24"`. If a host does not report a value that the expect query needs, the host is unknown, not "not done". See [Rules: tracking a rollout](user-guide.md#rules-tracking-a-rollout). |
| Command line | `kryostate host list -q '...'`, `kryostate collectors enable <id> --where '...'` and `kryostate rules add <name> --scope '...' --expect '...'` |

A query that selects the correct hosts on the **Inventory** page selects the
same hosts for a collector.

## Examples

```text
os.version == "AlmaLinux 9.8"            one exact version
os.family == "Linux" && stack == "web"   Linux hosts in the web stack
name =~ "^web"                           names that start with web
name in ("web01", "db01")                two hosts, by name
hw.cpu.cores >= 8                        eight cores or more
nginx.version < "1.24"                   older nginx, compared as versions
!has(os.version)                         hosts with no OS version
web                                      plain text: names that contain "web"
```

## Grammar

A query is made of **conditions**. A condition is usually a **name**, an
**operator** and a **value**. `&&` (and) and `||` (or) join conditions. `!`
(not) gives the opposite. Parentheses make groups.

```text
query      = or
or         = and { "||" and }
and        = unary { "&&" unary }
unary      = "!" unary | primary
primary    = "(" or ")"
           | "has" "(" field ")"
           | field "in" list
           | field [ comparison literal ]
field      = name [ "[" string "]" ]
list       = "(" literal { "," literal } [ "," ] ")"
           | "[" literal { "," literal } [ "," ] "]"
comparison = "==" | "!=" | "=~" | "!~" | "<" | "<=" | ">" | ">="
literal    = string | number | "true" | "false"
name       = letter { letter | digit | "_" | "." | "-" }
string     = '"' chars '"' | "'" chars "'" | "`" raw chars "`"
number     = [ "-" ] digit { digit } [ "." digit { digit } ]
```

- `&&` binds more tightly than `||`. Thus `a || b && c` means
  `a || (b && c)`. If you are not sure, use parentheses.
- `!` binds less tightly than a comparison. Thus `!os.family == "Linux"`
  means "not Linux".
- Spaces between the parts are not important.

### Plain text

If the text has none of the characters `=`, `!`, `<`, `>`, `~`, `&`, `|`,
`(`, `)`, `[`, `]`, `"`, `'` and `` ` ``, it is not a query. KryoState then finds the hosts whose name or identity (such
as the fqdn or an address) contains the text. Upper and lower case are not
important. For example, `web` and `db01` are plain text.

**CAUTION:** A name alone, such as `disabled` or `myapp.installed`, is plain
text in the **Inventory** box and in `kryostate host list -q`. To test a
yes-or-no value there, write `disabled == true` or `(disabled)`.

## Names

A name is a field of the host or an attribute.

| Name | Value |
|---|---|
| `name` | The host name |
| `id` | The internal ID of the host |
| `stack` | Each stack that the host is a member of |
| `role` | The role of the host in each of its stacks, and its `role` attribute if it has one |
| `application` or `app` | The application attribute of the host |
| `disabled` | `true` for a disabled host. KryoState lists a disabled host but does not connect to it. `disabled == true` selects these hosts, and `!disabled` selects the other hosts. |
| All other names | An attribute, such as `os.version`, `hw.cpu.cores` or `nginx.version` |

To find the name of an attribute:

- On the page of a host, put the pointer on a label in the **Facts** table.
  For example, **OS Version** is `os.version`.
- On the **Inventory** page, the **Columns** list shows all attributes that
  hosts have.
- Run `kryostate host show <host>`. It shows each attribute by name.
- On a collector page, **What it records** shows the attribute.

### Collectors that take a path or a name

The catalog collectors `file.exists`, `file.contents`, `config.value`,
`env.var` and `package.version` take a value. They record one attribute for
each value. For example, **File exists** for
`/etc/ssh/sshd_config.d/hardening.conf` records `file.hardening_conf.exists`.
You can use the attribute name, or the catalog name with the value in
brackets. These three queries are the same:

```text
file.hardening_conf.exists == true
file.exists["/etc/ssh/sshd_config.d/hardening.conf"] == true
file.exists == "/etc/ssh/sshd_config.d/hardening.conf"
```

The third query is a short form. `<catalog name> == "<value>"` means:

- for a collector that records `true` or `false`, such as `file.exists`: the
  value is `true`.
- for other collectors, such as `package.version == "openssl"`: the host has
  a value.

The short form also works with `!=` and with `in`, such as
`file.exists in ("/etc/a.conf", "/etc/b.conf")`. For `config.value`, the
value is `path#key` or `path#[section]key`.

If you gave the collector a different attribute name when you switched it on,
the bracket form finds that name.

## Operators

| Operator | Meaning |
|---|---|
| `==` `!=` | Equal and not equal. Text must match exactly, with upper and lower case. |
| `=~` `!~` | Matches and does not match a [Go regular expression](https://pkg.go.dev/regexp/syntax). The pattern can match at any position: `"web"` matches in all positions, `"^web"` only at the start and `"web$"` only at the end. To ignore upper and lower case, start the pattern with `(?i)`. |
| `<` `<=` `>` `>=` | Order. Numbers compare as numbers (`8 < 16`). Versions compare in version order (`"7.9" < "7.10"`, `"1.0.2" < "1.0.2k"`). A value that is not a number or a version does not match. |
| `in` | The value is one of a list: `stack in ("web", "edge")`. The rules are the same as for `==`. Use round or square brackets. |
| `has(x)` | The host has a value for `x`. |
| `x` alone | The same as `x == true`. `!x` is the same as `x != true`. |
| `&&` `\|\|` `!` `( )` | And, or, not, and groups |

## Values

| Value | Meaning |
|---|---|
| `"text"` or `'text'` | A string. The escapes are `\"`, `\'`, `` \` ``, `\\`, `\n`, `\t` and `\r`. |
| `` `text` `` | A raw string without escapes. Use it for patterns with backslashes: ``name =~ `^web\d+$` ``. |
| `8`, `-1.5` | A number. Put a version in quotes: `"1.2.3"`, not `1.2.3`. |
| `true`, `false` | Match attributes that record `true` or `false`, such as **File exists**. Old values `yes` and `no` also match. |

### Which value is compared

A query compares the **Value** column of the host page, not the **Raw**
column. KryoState sometimes makes a value shorter. For example,
`Debian GNU/Linux 12 (bookworm)` becomes `Debian 12`, and a full package
version becomes its first two numbers. The query sees the short form. To
match the full text, use a pattern: `os.version =~ "bookworm"`.

## How the edge cases work

- **A missing value is empty text.** `os.version == ""` finds the hosts
  without a value. `os.version != ""` and `has(os.version)` find the hosts
  with a value. A missing value does not match `<`, `<=`, `>` or `>=`. It
  matches a pattern only if the pattern matches empty text.
- **A name with many values matches if one value matches.** A host in the
  stacks `web` and `edge` matches `stack == "web"` and `stack == "edge"`.
- **`!=` and `!~` are the opposite of `==` and `=~`.** Thus `stack != "web"`
  means "not in the web stack", also for a host that is in a different stack
  too.
- **Order needs numbers or versions.** `hw.cpu.cores > 8` compares numbers.
  `os.kernel < "5"` compares versions. KryoState refuses `os.family < "M"`,
  because `"M"` is not a number or a version. A host value that is not a
  number or a version does not match an order operator.
- **Unknown names are not errors.** A name that no host has has no value.
  Thus `nginx.version != ""` finds the hosts that have nginx.

## Mistakes

KryoState does not run a query that does not parse. The **Inventory** page
shows the problem and its column. The **Change where it runs** box of a
collector shows a `^` under the problem.

| Query | Problem | Correct query |
|---|---|---|
| `os.family = "Linux"` | `=` is not an operator. | `os.family == "Linux"` |
| `os.family == Linux` | Text must be in quotes. | `os.family == "Linux"` |
| `name in ("web01" "web02")` | A comma is missing. | `name in ("web01", "web02")` |
| `file.exists == true` | The file is missing. | `file.exists["/etc/hosts"] == true` |
| `(stack == "web"` | The `)` is missing. | `(stack == "web")` |
| `os.family < "M"` | Order needs a number or a version. | `os.family =~ "^[A-L]"` |
| `name =~ "web\d"` | `\d` is not an escape in double quotes. | ``name =~ `web\d` `` |
| `os.version == 1.2.3` | A version must be in quotes. | `os.version == "1.2.3"` |

**CAUTION:** If the query of a collector does not parse, the collector runs
on no host. It does not run on all hosts. The collector page tells you why.

## Recipes

```text
# Hosts that have no OS version
!has(os.version)

# Linux hosts without a file that each Linux host must have
os.family == "Linux" && !file.exists == "/etc/ssh/sshd_config.d/hardening.conf"

# All hosts in two stacks, but not the databases
stack in ("web", "app") && role != "database"

# Old OpenSSL
pkg.openssl.version < "3.0"

# Large hosts that run a given application
application == "postgresql" && hw.memory.total_mb >= 16000

# Names that follow a pattern, with upper and lower case ignored
name =~ `(?i)^(web|edge)\d+`

# Some hosts, by name
name in ("web01", "web02", "app01")
```

## On the command line

```sh
kryostate host list -q 'os.family == "Linux" && stack == "web"'
kryostate host list -q 'hw.cpu.cores >= 8' --cols hw.cpu.cores,os.version
kryostate host list -q 'stack == "data"' --json > data.json
kryostate collectors enable nginx.version --where 'stack == "web"'
```

Put the query in single quotes, so that the shell does not change the double
quotes or the `!`. Single quotes work the same way in Windows PowerShell.

## Limits

KryoState runs queries on the values that it has loaded. It does not send a
query to the database, so a query cannot change data. A query reads only the
current values. To see how a value changed, use the **Changes** page.
