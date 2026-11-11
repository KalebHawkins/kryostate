# Example estate

This file stands in for a hand-maintained inventory. Each section states its
own total in prose, the way these documents usually do. The importer is
checked against those stated totals rather than against a number this project
wrote, so an importer that drops or duplicates rows is caught by the
document's own arithmetic.

## North site

Total: 4 servers.

| Host | Application | Role | Stack | OS | Address |
| --- | --- | --- | --- | --- | --- |
| `appnode1` | ledger | app | ledger-prod | Red Hat Enterprise Linux 8.6 (Ootpa) | 198.51.100.11 |
| `appnode2` | ledger | app | ledger-prod | Red Hat Enterprise Linux 8.6 (Ootpa) | 198.51.100.12 |
| `dbnode1` | datastore | database | ledger-prod | AIX 7.2 | 198.51.100.21 |
| `lbnode1` | proxy | loadbalancer | ledger-prod | Ubuntu 22.04.3 LTS | 198.51.100.31 |

## South site

Total: 3 servers.

| Host | Application | Role | Stack | OS | Address |
| --- | --- | --- | --- | --- | --- |
| `appnode3` | ledger | app | ledger-test | Red Hat Enterprise Linux 7.9 (Maipo) | 198.51.100.41 |
| `dbnode2` | datastore | database | ledger-test | 7200-05-04-2114 | 198.51.100.42 |
| `winnode1` | reporting | app | reporting-prod | Microsoft Windows Server 2019 Standard | 198.51.100.43 |

## Notes

Total: 0 servers.

This section has no table. An importer that counts headings rather than rows
would report something here.
