package ingest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/normalize"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Mapping says which column holds what. An inventory supplies the hosts, the
// stacks they belong to with their role in each, and the application each
// runs. Everything else about a host is what the host itself reports, so any
// other column is reported as not imported rather than recorded.
type Mapping struct {
	// HostColumn names the column holding the host identifier.
	HostColumn string `json:"host_column"`
	// FQDNColumn is optional and gives stronger identity evidence than a bare
	// name, which may repeat across locations.
	FQDNColumn string `json:"fqdn_column"`
	// AddressColumn is an address the document gives for the host. It is
	// recorded beside the addresses a resolver returns, never chosen as the
	// address to connect to.
	AddressColumn string `json:"address_column"`
	// StackColumn groups rows into a deployed system.
	StackColumn string `json:"stack_column"`
	// RoleColumn is the role the host plays in its stack.
	RoleColumn string `json:"role_column"`
	// ApplicationColumn is the application the host runs.
	ApplicationColumn string `json:"application_column"`
}

// columns lists the columns the mapping uses.
func (m Mapping) columns() map[string]bool {
	out := map[string]bool{}
	for _, c := range []string{m.HostColumn, m.FQDNColumn, m.AddressColumn,
		m.StackColumn, m.RoleColumn, m.ApplicationColumn} {
		if c != "" {
			out[c] = true
		}
	}
	return out
}

// Result counts what an import did.
type Result struct {
	Rows     int
	Created  int
	Matched  int
	Asserted int
	Skipped  int
	// Ignored lists the columns the document has that were not imported,
	// so a column that used to be read is not dropped without a word.
	Ignored   []string
	BySection map[string]int
	Problems  []string
}

// Importer writes a document's hosts, stacks, roles and applications into
// the store.
type Importer struct {
	Store *store.Store
	Norm  *normalize.Normalizer
	// Name identifies the document as the source of what it says.
	Name string
}

// Import records every row: the host, its address, its stack and role, and
// its application.
//
// The application is a document claim, so a collector that reports one wins
// over it. Hostname is used as identity evidence only after any stronger
// evidence in the row, because a bare name repeats across locations and a
// name is the one field people retype by hand.
func (im *Importer) Import(ctx context.Context, tables []Table, m Mapping) (Result, error) {
	res := Result{BySection: map[string]int{}}
	if m.HostColumn == "" {
		return res, fmt.Errorf("mapping needs a host column")
	}
	srcID, err := im.Store.SourceID(ctx, model.SourceDocument, im.Name)
	if err != nil {
		return res, err
	}
	observed := time.Now()
	used := m.columns()
	ignored := map[string]bool{}

	for _, t := range tables {
		if !hasColumn(t.Headers, m.HostColumn) {
			continue
		}
		for _, h := range t.Headers {
			if !used[h] {
				ignored[h] = true
			}
		}
		for _, row := range t.RowMaps() {
			res.Rows++
			host := strings.TrimSpace(row.Values[m.HostColumn])
			if host == "" {
				res.Skipped++
				continue
			}
			cell := func(col string) string {
				if col == "" {
					return ""
				}
				return strings.TrimSpace(row.Values[col])
			}

			evidence := []model.Identity{}
			if fqdn := cell(m.FQDNColumn); fqdn != "" {
				evidence = append(evidence, model.Identity{Scheme: model.IdentFQDN, Value: strings.ToLower(fqdn)})
			}
			evidence = append(evidence, model.Identity{Scheme: model.IdentHostname, Value: strings.ToLower(host)})

			ciID, created, err := im.Store.ResolveCI(ctx, model.KindHost, host, evidence)
			if err != nil {
				res.Problems = append(res.Problems, fmt.Sprintf("%s: %v", host, err))
				res.Skipped++
				continue
			}
			if created {
				res.Created++
			} else {
				res.Matched++
			}
			res.BySection[row.Section]++

			if addr := cell(m.AddressColumn); addr != "" {
				family := "ipv4"
				if strings.Contains(addr, ":") {
					family = "ipv6"
				}
				if err := im.Store.RecordAddress(ctx, ciID, addr, family, im.Name, observed); err != nil {
					return res, err
				}
			}

			if app := cell(m.ApplicationColumn); app != "" {
				var n *store.Normalized
				if nr, ok := im.Norm.Normalize(model.AttrApplication, app); ok {
					n = &store.Normalized{
						Value: nr.Value, Family: nr.Family, Major: nr.Major,
						Minor: nr.Minor, Patch: nr.Patch, RuleID: nr.RuleID,
						Version: im.Norm.Version,
					}
				}
				if _, err := im.Store.Assert(ctx, model.Assertion{
					CIID: ciID, Attr: model.AttrApplication, Raw: app, SourceID: srcID,
					ValueType: model.TypeText, ObservedAt: observed,
				}, n); err != nil {
					return res, err
				}
				res.Asserted++
			}

			role := cell(m.RoleColumn)
			if name := cell(m.StackColumn); name != "" {
				id, err := im.Store.EnsureStack(ctx, name)
				if err != nil {
					return res, err
				}
				if err := im.Store.AddToStack(ctx, id, ciID, role); err != nil {
					return res, err
				}
			} else if role != "" {
				// A role is a part played in a stack. With no stack named
				// there is nothing to hold it.
				res.Problems = append(res.Problems,
					fmt.Sprintf("%s: role %q not recorded, the row names no stack", host, role))
			}
		}
	}
	for h := range ignored {
		res.Ignored = append(res.Ignored, h)
	}
	sort.Strings(res.Ignored)
	return res, im.Store.Audit(ctx, "import", "import_document", im.Name,
		fmt.Sprintf("%d rows, %d new, %d matched, %d applications", res.Rows, res.Created, res.Matched, res.Asserted))
}

func hasColumn(headers []string, name string) bool {
	for _, h := range headers {
		if h == name {
			return true
		}
	}
	return false
}
