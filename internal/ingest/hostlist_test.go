package ingest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/store"
)

func TestParseHostListReadsWhatPeoplePaste(t *testing.T) {
	got, problems := ParseHostList(`
# payments
web01 app
web02, app, web02.example.com
DB01.EXAMPLE.COM	database
lb01:loadbalancer
solo
web03 app server
`)
	want := []HostLine{
		{Line: 3, Name: "web01", Role: "app"},
		{Line: 4, Name: "web02", FQDN: "web02.example.com", Role: "app"},
		{Line: 5, Name: "db01", FQDN: "db01.example.com", Role: "database"},
		{Line: 6, Name: "lb01", Role: "loadbalancer"},
		{Line: 7, Name: "solo"},
		{Line: 8, Name: "web03", Role: "app server"},
	}
	if !reflect.DeepEqual(got, want) || len(problems) != 0 {
		t.Errorf("got %+v, problems %v", got, problems)
	}
}

func TestAddHostsToStack(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	lines, _ := ParseHostList("web01 app web01.example.invalid\ndb01 database")
	res, err := AddHostsToStack(ctx, st, "payments", lines, BulkOptions{Create: true, User: "svc-ro", By: "op"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Created != 2 || res.Connected != 2 || len(res.Problems) != 0 {
		t.Fatalf("result %+v", res)
	}
	id, _ := st.LookupCI(ctx, "web01.example.invalid")
	ps, _ := st.ConnProfiles(ctx, id)
	if len(ps) != 1 || ps[0].Address != "web01.example.invalid" || ps[0].CredentialRef != "agent:" {
		t.Errorf("connection %+v", ps)
	}
	ms, _ := st.MembershipsOf(ctx, id)
	if len(ms) != 1 || ms[0].Role != "app" {
		t.Errorf("membership %+v", ms)
	}

	// Again, without Create: existing hosts are found, unknown ones reported,
	// and nobody gets a second connection.
	lines, _ = ParseHostList("web01 web\nghost99")
	res, err = AddHostsToStack(ctx, st, "payments", lines, BulkOptions{User: "svc-ro"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Created != 0 || res.Connected != 0 || len(res.Problems) != 1 ||
		!strings.Contains(res.Problems[0], "ghost99") {
		t.Errorf("second pass %+v", res)
	}
	if ms, _ := st.MembershipsOf(ctx, id); ms[0].Role != "web" {
		t.Errorf("the role was not updated: %+v", ms)
	}
}
