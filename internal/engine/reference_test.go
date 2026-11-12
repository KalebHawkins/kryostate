package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

func TestExternalDefinitionLoadsBack(t *testing.T) {
	ex := collect.External{Name: "vmware", Path: "/opt/kryostate/vmware.sh", Args: []string{"-v"},
		Timeout: 90 * time.Second, Comment: "asks vCenter"}
	b, err := ExternalJSON(ex)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "externals.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadExternals(p)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], ex) {
		t.Errorf("wrote %+v, read %+v", ex, got)
	}
}

func TestDefinitionFindsEveryKind(t *testing.T) {
	specs, err := collect.BuiltinSpecs()
	if err != nil {
		t.Fatal(err)
	}
	if err := specs.Merge([]byte(`{"version":1,"specs":[{"id":"my.thing","attr":"my.thing","os":["linux"],"command":"uname -n","parse":{"kind":"first_line"}}]}`)); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Specs: specs, Externals: []collect.External{{Name: "vm", Path: "/bin/vm", Env: []string{"TOKEN=s3cret"}}}}
	if own := e.OwnSpecs(); len(own) != 1 || own[0].ID != "my.thing" {
		t.Errorf("own specs %+v", own)
	}
	for id, want := range map[string]string{
		"cpu.cores":          `"id": "cpu.cores.linux"`,
		"file.exists":        `'/etc/resolv.conf'`,
		"builtin.os.version": `"id": "linux.os.version"`,
		"spec.my.thing":      `"command": "uname -n"`,
		"linux.os.family":    `"id": "linux.os.family"`,
		"external.vm":        `"TOKEN=(hidden)"`,
	} {
		file, b, err := e.Definition(id, "")
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s (%s) lacks %s:\n%s", id, file, want, b)
		}
		if strings.Contains(string(b), "s3cret") {
			t.Errorf("%s shows an external program's secret", id)
		}
	}
	if _, _, err := e.Definition("made.up", ""); err == nil {
		t.Error("an unknown id gave a definition")
	}
}
