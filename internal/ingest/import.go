package ingest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/normalize"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Mapping says which column holds the host name and what attribute each other
// column claims. Columns with no mapping are ignored rather than guessed at.
type Mapping struct {
	// HostColumn names the column holding the host identifier.
	HostColumn string `json:"host_column"`
	// FQDNColumn is optional and gives stronger identity evidence than a bare
	// name, which may repeat across locations.
	FQDNColumn string `json:"fqdn_column"`
	// StackColumn groups rows into a deployed system.
	StackColumn string `json:"stack_column"`
	// AddressColumn records an address the document claims. It is recorded as
	// a claim, never as a management endpoint.
	AddressColumn string `json:"address_column"`
	// Attrs maps a column header to an attribute name.
	Attrs map[string]string `json:"attrs"`
	// SectionAttr records the heading a row appeared under, if set. A written
	// inventory grouped by location is claiming that location, and that claim
	// should be visible and contestable like any other.
	SectionAttr string `json:"section_attr"`
}

// Result counts what an import did.
type Result struct {
	Rows      int
	Created   int
	Matched   int
	Asserted  int
	Skipped   int
	BySection map[string]int
	Problems  []string
}

// Importer writes a document's claims into the store.
type Importer struct {
	Store *store.Store
	Norm  *normalize.Normalizer
	// Name identifies the document, so two inventories can disagree with each
	// other as well as with the machines.
	Name string
}

// Import records every row as claims from the named document.
//
// Nothing here is treated as authoritative. A document claim loses to a
// collected value under the default precedence, and where the two differ the
// detection pass raises it. Hostname is used as identity evidence only after
// any stronger evidence in the row, because a bare name repeats across
// locations and a name is the one field people retype by hand.
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

	for _, t := range tables {
		if !hasColumn(t.Headers, m.HostColumn) {
			continue
		}
		for _, row := range t.RowMaps() {
			res.Rows++
			host := strings.TrimSpace(row.Values[m.HostColumn])
			if host == "" {
				res.Skipped++
				continue
			}

			evidence := []model.Identity{}
			if m.FQDNColumn != "" {
				if fqdn := strings.TrimSpace(row.Values[m.FQDNColumn]); fqdn != "" {
					evidence = append(evidence, model.Identity{Scheme: model.IdentFQDN, Value: strings.ToLower(fqdn)})
				}
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

			claims := map[string]string{}
			for col, attr := range m.Attrs {
				if v := strings.TrimSpace(row.Values[col]); v != "" {
					claims[attr] = v
				}
			}
			if m.SectionAttr != "" && row.Section != "" {
				claims[m.SectionAttr] = row.Section
			}
			if m.AddressColumn != "" {
				if v := strings.TrimSpace(row.Values[m.AddressColumn]); v != "" {
					claims["net.documented_address"] = v
				}
			}

			for attr, v := range claims {
				var n *store.Normalized
				if nr, ok := im.Norm.Normalize(attr, v); ok {
					n = &store.Normalized{
						Value: nr.Value, Family: nr.Family, Major: nr.Major,
						Minor: nr.Minor, Patch: nr.Patch, RuleID: nr.RuleID,
						Version: im.Norm.Version,
					}
				}
				if _, err := im.Store.Assert(ctx, model.Assertion{
					CIID: ciID, Attr: attr, Raw: v, SourceID: srcID,
					ValueType: typeFor(attr), ObservedAt: observed,
				}, n); err != nil {
					return res, err
				}
				res.Asserted++
			}

			if m.StackColumn != "" {
				if name := strings.TrimSpace(row.Values[m.StackColumn]); name != "" {
					id, err := im.Store.EnsureStack(ctx, name)
					if err != nil {
						return res, err
					}
					role := claims["role"]
					if err := im.Store.AddToStack(ctx, id, ciID, role); err != nil {
						return res, err
					}
				}
			}
		}
	}
	return res, im.Store.Audit(ctx, "import", "import_document", im.Name,
		fmt.Sprintf("%d rows, %d new, %d matched, %d claims", res.Rows, res.Created, res.Matched, res.Asserted))
}

// typeFor guesses how a documented value should be compared. It is a guess,
// which is why it only ever affects presentation and never which value wins.
func typeFor(attr string) model.ValueType {
	switch {
	case strings.HasSuffix(attr, ".version"), attr == "os.version":
		return model.TypeVersion
	case strings.HasSuffix(attr, ".path"), strings.HasSuffix(attr, "_home"):
		return model.TypePath
	default:
		return model.TypeText
	}
}

func hasColumn(headers []string, name string) bool {
	for _, h := range headers {
		if h == name {
			return true
		}
	}
	return false
}
