package ingest

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/normalize"
	"github.com/KalebHawkins/kryostate/internal/store"
)

const docPath = "../../testdata/documents/example-inventory.md"

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mapping() Mapping {
	return Mapping{
		HostColumn:        "Host",
		StackColumn:       "Stack",
		AddressColumn:     "Address",
		RoleColumn:        "Role",
		ApplicationColumn: "Application",
	}
}

// statedTotals reads the totals the document states about itself, in prose,
// under each heading. These numbers were written by whoever wrote the
// document; nothing in this project computed them.
func statedTotals(t *testing.T, raw string) map[string]int {
	t.Helper()
	heading := regexp.MustCompile(`(?m)^##\s+(.+?)\s*$`)
	total := regexp.MustCompile(`(?m)^Total:\s+(\d+)\s+servers\.`)

	out := map[string]int{}
	section := ""
	for _, line := range strings.Split(raw, "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		if m := total.FindStringSubmatch(line); m != nil && section != "" {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatal(err)
			}
			out[section] = n
		}
	}
	if len(out) == 0 {
		t.Fatal("the fixture states no totals, so this test proves nothing")
	}
	return out
}

// TestImportReproducesTheDocumentsOwnTotals checks the importer against
// arithmetic it did not author. If a row is dropped, double-counted, or
// attached to the wrong heading, the section counts stop matching the totals
// printed in the document itself.
func TestImportReproducesTheDocumentsOwnTotals(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	want := statedTotals(t, string(raw))

	tables, err := ParseMarkdown(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	im := &Importer{Store: testStore(t), Norm: n, Name: "example-inventory.md"}

	res, err := im.Import(ctx, tables, mapping())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 0 {
		t.Errorf("import reported problems: %v", res.Problems)
	}

	sum := 0
	for section, count := range want {
		sum += count
		if got := res.BySection[section]; got != count {
			t.Errorf("section %q: imported %d hosts, the document says %d", section, got, count)
		}
	}
	for section, got := range res.BySection {
		if _, ok := want[section]; !ok {
			t.Errorf("imported %d hosts under %q, which states no total", got, section)
		}
	}
	if res.Rows != sum {
		t.Errorf("parsed %d rows, the document's totals add up to %d", res.Rows, sum)
	}
	if res.Created != sum {
		t.Errorf("created %d hosts from %d rows; identity resolution merged or forked something", res.Created, sum)
	}
}

// TestSecondImportCreatesNothingNew is the same document twice. Re-importing
// an unchanged file is the normal case, and it must not duplicate the estate
// or bury real history under identical repeated claims.
func TestSecondImportCreatesNothingNew(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := ParseMarkdown(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	im := &Importer{Store: testStore(t), Norm: n, Name: "example-inventory.md"}

	first, err := im.Import(ctx, tables, mapping())
	if err != nil {
		t.Fatal(err)
	}
	second, err := im.Import(ctx, tables, mapping())
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 {
		t.Errorf("re-importing the same document created %d hosts", second.Created)
	}
	if second.Matched != first.Created {
		t.Errorf("matched %d of %d known hosts on the second pass", second.Matched, first.Created)
	}

	hosts, err := im.Store.Hosts(ctx, store.HostFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != first.Created {
		t.Errorf("store holds %d hosts after importing %d rows twice", len(hosts), first.Created)
	}
}

// TestDocumentedAddressIsNotAnEndpoint pins a deliberate omission. An address
// written in a document is a claim about a host, and recording it as the way
// to reach that host would make a typo into a connection to someone else's
// machine.
func TestDocumentedAddressIsNotAnEndpoint(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := ParseMarkdown(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	s := testStore(t)
	im := &Importer{Store: s, Norm: n, Name: "example-inventory.md"}
	if _, err := im.Import(ctx, tables, mapping()); err != nil {
		t.Fatal(err)
	}

	hosts, err := s.Hosts(ctx, store.HostFilter{Search: "appnode1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected one host, got %d", len(hosts))
	}
	addrs, err := s.Addresses(ctx, hosts[0].CI.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if a.Selected {
			t.Errorf("import marked %s as the address to connect to", a.Addr)
		}
	}

	facts, err := s.Facts(ctx, hosts[0].CI.ID)
	if err != nil {
		t.Fatal(err)
	}
	byAttr := map[string]model.Fact{}
	for _, f := range facts {
		byAttr[f.Attr] = f
	}
	if len(addrs) != 1 || addrs[0].Addr != "198.51.100.11" {
		t.Errorf("the documented address was not recorded: %+v", addrs)
	}
	if got := byAttr[model.AttrApplication].Raw; got != "ledger" {
		t.Errorf("application is %q, want ledger", got)
	}
	// Only the application is an attribute a document states. The operating
	// system in the same row is the host's to report.
	if len(facts) != 1 {
		t.Errorf("import recorded %d attributes, want only the application: %+v", len(facts), facts)
	}
	ms, err := s.MembershipsOf(ctx, hosts[0].CI.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Stack != "ledger-prod" || ms[0].Role != "app" {
		t.Errorf("stack membership %+v, want ledger-prod as app", ms)
	}
}

// TestSectionWithNoTableContributesNothing guards against counting headings
// instead of rows.
func TestSectionWithNoTableContributesNothing(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := ParseMarkdown(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		if tb.Section == "Notes" {
			t.Fatalf("parsed a table under a section that has none: %+v", tb)
		}
	}
	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	im := &Importer{Store: testStore(t), Norm: n, Name: "doc"}
	res, err := im.Import(ctx, tables, mapping())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.BySection["Notes"]; ok {
		t.Error("a prose section was counted as hosts")
	}
}

// TestRowWithNoHostIsSkippedNotInvented keeps a blank identifier from becoming
// a host record named after nothing.
func TestRowWithNoHostIsSkippedNotInvented(t *testing.T) {
	ctx := context.Background()
	doc := strings.Join([]string{
		"## Site",
		"",
		"| Host | Role |",
		"| --- | --- |",
		"| appnode1 | app |",
		"|  | app |",
		"",
	}, "\n")
	tables, err := ParseMarkdown(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	im := &Importer{Store: testStore(t), Norm: n, Name: "doc"}
	res, err := im.Import(ctx, tables, Mapping{HostColumn: "Host", RoleColumn: "Role"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 2 {
		t.Errorf("read %d rows, want 2", res.Rows)
	}
	if res.Created != 1 || res.Skipped != 1 {
		t.Errorf("created %d, skipped %d; want 1 and 1", res.Created, res.Skipped)
	}
	// The row names a role and no stack to play it in, which is said rather
	// than dropped.
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "no stack") {
		t.Errorf("a role with no stack was not reported: %v", res.Problems)
	}
}

// TestIgnoredColumnsAreReported: an inventory states hosts, stacks, roles and
// applications. A column it has beyond those is named in the result, so
// somebody whose spreadsheet used to set the operating system is told it no
// longer does.
func TestIgnoredColumnsAreReported(t *testing.T) {
	ctx := context.Background()
	doc := "## Site\n\n| Host | OS | Owner | Application |\n| --- | --- | --- | --- |\n| appnode1 | RHEL 7.9 | ops | ledger |\n"
	tables, err := ParseMarkdown(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	s := testStore(t)
	im := &Importer{Store: s, Norm: n, Name: "doc"}
	res, err := im.Import(ctx, tables, Mapping{HostColumn: "Host", ApplicationColumn: "Application"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Ignored, ",") != "OS,Owner" {
		t.Errorf("ignored columns %v, want OS and Owner", res.Ignored)
	}
	if res.Asserted != 1 {
		t.Errorf("recorded %d values, want only the application", res.Asserted)
	}
}

// TestDocumentLosesToCollector is the precedence the tool rests on: what a
// host reports is the truth, and an application written in a document stands
// only until the host says otherwise.
func TestDocumentLosesToCollector(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}

	doc := "## Site\n\n| Host | Application |\n| --- | --- |\n| appnode1 | ledger |\n"
	tables, err := ParseMarkdown(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	im := &Importer{Store: s, Norm: n, Name: "doc"}
	if _, err := im.Import(ctx, tables, Mapping{HostColumn: "Host", ApplicationColumn: "Application"}); err != nil {
		t.Fatal(err)
	}

	hosts, err := s.Hosts(ctx, store.HostFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected one host, got %d", len(hosts))
	}
	if got := hosts[0].Values[model.AttrApplication]; got != "ledger" {
		t.Errorf("application before collection is %q, want the document's", got)
	}
	col, err := s.SourceID(ctx, model.SourceCollector, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assert(ctx, model.Assertion{
		CIID: hosts[0].CI.ID, Attr: model.AttrApplication, Raw: "ledger2", SourceID: col,
		ObservedAt: time.Now().Add(-time.Hour),
	}, nil); err != nil {
		t.Fatal(err)
	}

	facts, err := s.Facts(ctx, hosts[0].CI.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Attr == model.AttrApplication && f.Raw != "ledger2" {
			t.Errorf("resolved application is %q; the document beat the machine", f.Raw)
		}
	}
}
