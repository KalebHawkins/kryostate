package ingest

import (
	"fmt"
	"sort"
	"strings"
)

// commonColumns are header names that mean the same thing in most
// hand-kept inventories. Matching ignores case, spaces, dashes and
// underscores, so "Host Name", "host_name" and "HOSTNAME" are one header.
var commonColumns = struct {
	host, fqdn, stack, address []string
	attrs                      map[string]string
}{
	host:    []string{"host", "hostname", "server", "name", "servername", "machine"},
	fqdn:    []string{"fqdn", "dnsname", "fullname"},
	stack:   []string{"stack", "system", "service"},
	address: []string{"address", "ip", "ipaddress", "ipv4"},
	attrs: map[string]string{
		"role": "role", "function": "role",
		"os": "os.version", "operatingsystem": "os.version", "osversion": "os.version",
		"application": "app.name", "app": "app.name", "appname": "app.name",
		"appversion": "app.version", "version": "app.version",
		"environment": "env", "env": "env",
		"owner": "owner", "team": "owner",
		"site": "site", "location": "site", "datacenter": "site",
		"model": "hw.model", "serial": "hw.serial", "serialnumber": "hw.serial",
	},
}

func squash(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.NewReplacer(" ", "", "-", "", "_", "", ".", "").Replace(h)
}

// GuessMapping works out a mapping from the tables' headers, for a document
// imported without a mapping file. It returns lines describing each decision
// so the person can see what was understood, and the headers it ignored.
func GuessMapping(tables []Table) (Mapping, []string, error) {
	m := Mapping{Attrs: map[string]string{}}
	seen := map[string]bool{}
	var headers []string
	for _, t := range tables {
		for _, h := range t.Headers {
			if !seen[h] {
				seen[h] = true
				headers = append(headers, h)
			}
		}
	}
	pick := func(names []string, h string) bool {
		for _, n := range names {
			if squash(h) == n {
				return true
			}
		}
		return false
	}
	var notes, ignored []string
	for _, h := range headers {
		switch {
		case m.HostColumn == "" && pick(commonColumns.host, h):
			m.HostColumn = h
			notes = append(notes, fmt.Sprintf("%q names the host", h))
		case m.FQDNColumn == "" && pick(commonColumns.fqdn, h):
			m.FQDNColumn = h
			notes = append(notes, fmt.Sprintf("%q is its fully qualified name", h))
		case m.StackColumn == "" && pick(commonColumns.stack, h):
			m.StackColumn = h
			notes = append(notes, fmt.Sprintf("%q is the stack it belongs to", h))
		case m.AddressColumn == "" && pick(commonColumns.address, h):
			m.AddressColumn = h
			notes = append(notes, fmt.Sprintf("%q is its address", h))
		default:
			if attr, ok := commonColumns.attrs[squash(h)]; ok {
				m.Attrs[h] = attr
				notes = append(notes, fmt.Sprintf("%q is recorded as %s", h, attr))
			} else {
				ignored = append(ignored, h)
			}
		}
	}
	if m.HostColumn == "" {
		return m, notes, fmt.Errorf("no column looks like the host name (tried %s); name it Host, or write a mapping file and pass -map",
			strings.Join(commonColumns.host, ", "))
	}
	if len(ignored) > 0 {
		sort.Strings(ignored)
		notes = append(notes, "not imported: "+strings.Join(ignored, ", ")+" (write a mapping file to include them)")
	}
	return m, notes, nil
}
