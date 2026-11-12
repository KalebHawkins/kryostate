# Lab inventory

A hand-kept list, the kind KryoState is meant to check. It is deliberately a
little wrong: idx01 and web01 were upgraded after it was written, and hf01
was planned but never built.

| Host | Stack | Role | Application | OS | Environment | Owner |
| --- | --- | --- | --- | --- | --- | --- |
| uf01 | splunk-lab | forwarder | splunk | Debian GNU/Linux 12 (bookworm) | lab | observability |
| uf02 | splunk-lab | forwarder | splunk | AlmaLinux 9.8 (Olive Jaguar) | lab | observability |
| idx01 | splunk-lab | indexer | splunk | AlmaLinux 9.6 (Sage Margay) | lab | observability |
| hf01 | splunk-lab | heavy-forwarder | splunk | AlmaLinux 9.8 (Olive Jaguar) | lab | observability |
| web01 | web | web | nginx | Debian GNU/Linux 11 (bullseye) | lab | web-team |
