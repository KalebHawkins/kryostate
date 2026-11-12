package ingest

import (
	"strings"
	"testing"
)

func TestGuessMapping(t *testing.T) {
	tables := []Table{{Headers: []string{"Host Name", "IP", "Stack", "Operating System", "Role", "Application", "Owner", "Notes"}}}
	m, notes, err := GuessMapping(tables)
	if err != nil {
		t.Fatal(err)
	}
	if m.HostColumn != "Host Name" || m.AddressColumn != "IP" || m.StackColumn != "Stack" {
		t.Errorf("columns %+v", m)
	}
	if m.RoleColumn != "Role" || m.ApplicationColumn != "Application" {
		t.Errorf("role and application %+v", m)
	}
	// Operating system and owner are not stated by a document any more: the
	// host reports the one, and the other is not an attribute at all. Both
	// are named as not imported rather than dropped without a word.
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "not imported: Notes, Operating System, Owner") {
		t.Errorf("ignored columns were not listed: %v", notes)
	}
	if _, _, err := GuessMapping([]Table{{Headers: []string{"Thing", "Colour"}}}); err == nil {
		t.Error("a table with no host column was accepted")
	}
}
