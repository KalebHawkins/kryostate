package collect

import (
	"reflect"
	"strings"
	"testing"
)

// TestEveryCatalogDefinitionLoads: the definition the collectors page shows
// for each catalog entry is offered as something to copy into collectors.json,
// so it must load there unchanged, alongside the built-in specs, and mean
// exactly what was written out.
func TestEveryCatalogDefinitionLoads(t *testing.T) {
	for _, tmpl := range Catalog {
		specs, err := tmpl.Definition("")
		if err != nil {
			t.Errorf("%s: %v", tmpl.ID, err)
			continue
		}
		b, err := SpecsJSON(specs)
		if err != nil {
			t.Fatalf("%s: %v", tmpl.ID, err)
		}
		set, err := LoadSpecs(b)
		if err != nil {
			t.Errorf("%s: the definition does not load: %v\n%s", tmpl.ID, err, b)
			continue
		}
		if !reflect.DeepEqual(set.Specs, specs) {
			t.Errorf("%s: the definition changed on loading:\nwrote %+v\nread  %+v", tmpl.ID, specs, set.Specs)
		}
		builtin, err := BuiltinSpecs()
		if err != nil {
			t.Fatal(err)
		}
		if err := builtin.Merge(b); err != nil {
			t.Errorf("%s: the definition clashes with the built-in specs: %v", tmpl.ID, err)
		}
		if tmpl.Example() == "" {
			t.Errorf("%s has no example value", tmpl.ID)
		}
		for _, prefix := range []string{RefBuiltin, RefSpec, RefExternal} {
			if strings.HasPrefix(tmpl.ID, prefix) {
				t.Errorf("catalog id %s starts with the reserved prefix %s", tmpl.ID, prefix)
			}
		}
	}
}

func TestDefinitionUsesTheGivenValue(t *testing.T) {
	tmpl, _ := Lookup("config.value")
	specs, err := tmpl.Definition("")
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Attr != "conf.app_conf.database.host" || !strings.Contains(specs[0].Command, "'host'") {
		t.Errorf("example definition %+v", specs[0])
	}
	tmpl, _ = Lookup("file.exists")
	specs, _ = tmpl.Definition("/etc/hosts")
	if specs[0].ID != "file.exists.unix" || specs[0].Attr != "file.hosts.exists" || !strings.Contains(specs[0].Command, "'/etc/hosts'") {
		t.Errorf("definition %+v", specs[0])
	}
	if _, err := tmpl.Definition("/etc/x'; reboot"); err == nil {
		t.Error("a value that is not a path was written into a definition")
	}
}

func TestSpecsJSONKeepsShortPartsOnOneLine(t *testing.T) {
	b, err := SpecsJSON([]Spec{{ID: "a.b", Attr: "a.b", OS: []string{"linux", "aix"}, Command: "x 2>&1 && y",
		Parse: Parse{Kind: ParseRegex, Pattern: `v(\d+)`}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"os": ["linux", "aix"]`, `"parse": { "kind": "regex", "pattern": "v(\\d+)" }`, `"command": "x 2>&1 && y"`, "{\n  \"version\": 1,\n  \"specs\": [\n    {\n      \"id\": \"a.b\","} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestBuiltinRefsGroupByAttribute(t *testing.T) {
	order, by, err := BuiltinRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) == 0 || len(by["os.version"]) < 3 {
		t.Errorf("order %v, os.version %d specs", order, len(by["os.version"]))
	}
}
