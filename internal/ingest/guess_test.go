package ingest

import (
	"strings"
	"testing"
)

func TestGuessMapping(t *testing.T) {
	tables := []Table{{Headers: []string{"Host Name", "IP", "Stack", "Operating System", "Role", "Owner", "Notes"}}}
	m, notes, err := GuessMapping(tables)
	if err != nil {
		t.Fatal(err)
	}
	if m.HostColumn != "Host Name" || m.AddressColumn != "IP" || m.StackColumn != "Stack" {
		t.Errorf("columns %+v", m)
	}
	if m.Attrs["Operating System"] != "os.version" || m.Attrs["Role"] != "role" || m.Attrs["Owner"] != "owner" {
		t.Errorf("attrs %v", m.Attrs)
	}
	if _, ok := m.Attrs["Notes"]; ok || !strings.Contains(strings.Join(notes, "\n"), "not imported: Notes") {
		t.Errorf("Notes was not left out visibly: %v", notes)
	}
	if _, _, err := GuessMapping([]Table{{Headers: []string{"Thing", "Colour"}}}); err == nil {
		t.Error("a table with no host column was accepted")
	}
}
