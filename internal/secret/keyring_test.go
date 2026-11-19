package secret

import "testing"

// TestEntryFromName: a stored name splits at its last colon, so a service
// that holds a whole reference by mistake still names its own entry, and is
// marked as a mistake.
func TestEntryFromName(t *testing.T) {
	for name, want := range map[string]KeyringEntry{
		"kryostate:ops":         {Name: "kryostate:ops", Service: "kryostate", Account: "ops"},
		"keyring:kryostate:ops": {Name: "keyring:kryostate:ops", Service: "keyring:kryostate", Account: "ops"},
	} {
		got, ok := EntryFromName(name)
		if !ok || got != want {
			t.Errorf("EntryFromName(%q) = %+v %v, want %+v", name, got, ok, want)
		}
	}
	for _, bad := range []string{"", "nocolon", ":ops", "kryostate:"} {
		if _, ok := EntryFromName(bad); ok {
			t.Errorf("EntryFromName(%q) accepted", bad)
		}
	}
	if e, _ := EntryFromName("kryostate:ops"); e.Mistaken() || e.Ref() != "keyring:kryostate/ops" {
		t.Errorf("a correct entry: mistaken %v, ref %s", e.Mistaken(), e.Ref())
	}
	if e, _ := EntryFromName("keyring:kryostate:ops"); !e.Mistaken() {
		t.Error("an entry named after a whole reference is not marked as a mistake")
	}
}
