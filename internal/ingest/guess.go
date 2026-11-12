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
	host, fqdn, stack, address, role, application []string
}{
	host:        []string{"host", "hostname", "server", "name", "servername", "machine"},
	fqdn:        []string{"fqdn", "dnsname", "fullname"},
	stack:       []string{"stack", "system", "service"},
	address:     []string{"address", "ip", "ipaddress", "ipv4"},
	role:        []string{"role", "function"},
	application: []string{"application", "app", "appname"},
}

func squash(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.NewReplacer(" ", "", "-", "", "_", "", ".", "").Replace(h)
}

// GuessMapping works out a mapping from the tables' headers, for a document
// imported without a mapping file. It returns lines describing each decision
// so the person can see what was understood, and the headers it ignored.
// Only the host, its names and address, its stack and role, and its
// application are read from a document; any other column is listed as not
// imported.
func GuessMapping(tables []Table) (Mapping, []string, error) {
	var m Mapping
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
		case m.RoleColumn == "" && pick(commonColumns.role, h):
			m.RoleColumn = h
			notes = append(notes, fmt.Sprintf("%q is its role in the stack", h))
		case m.ApplicationColumn == "" && pick(commonColumns.application, h):
			m.ApplicationColumn = h
			notes = append(notes, fmt.Sprintf("%q is the application it runs", h))
		default:
			ignored = append(ignored, h)
		}
	}
	if m.HostColumn == "" {
		return m, notes, fmt.Errorf("no column looks like the host name (tried %s); name it Host, or write a mapping file and pass -map",
			strings.Join(commonColumns.host, ", "))
	}
	if len(ignored) > 0 {
		sort.Strings(ignored)
		notes = append(notes, "not imported: "+strings.Join(ignored, ", ")+
			" (an inventory supplies hosts, addresses, stacks, roles and applications; everything else is collected from the host)")
	}
	return m, notes, nil
}
