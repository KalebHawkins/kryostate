package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

func okEnvelope() Envelope {
	return Envelope{
		Schema:    EnvelopeSchema,
		Collector: "example",
		Status:    "ok",
		Facts: []FactItem{
			{Key: "product.version", Value: "4.2.1", Type: model.TypeVersion},
			{Key: "product.home", Value: "/opt/product", Type: model.TypePath},
		},
	}
}

// TestValidateIsAllOrNothing is the rule that keeps a buggy collector visible.
// Nine good facts and one bad one is a bug, and taking the nine hides it until
// somebody acts on the tenth.
func TestValidateIsAllOrNothing(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Envelope)
		want string
	}{
		{"good", func(*Envelope) {}, ""},
		{"wrong schema", func(e *Envelope) { e.Schema = "hearsay.facts/v2" }, "schema"},
		{"no schema", func(e *Envelope) { e.Schema = "" }, "schema"},
		{"unnamed collector", func(e *Envelope) { e.Collector = "" }, "collector"},
		{"invented status", func(e *Envelope) { e.Status = "partial" }, "status"},
		{"bare key", func(e *Envelope) { e.Facts[1].Key = "version" }, "namespaced"},
		{"shouty key", func(e *Envelope) { e.Facts[1].Key = "Product.Version" }, "namespaced"},
		{"key with a space", func(e *Envelope) { e.Facts[1].Key = "product. version" }, "namespaced"},
		{"empty key", func(e *Envelope) { e.Facts[1].Key = "" }, "namespaced"},
		{"duplicate key", func(e *Envelope) { e.Facts[1].Key = e.Facts[0].Key }, "twice"},
		{"overlong key", func(e *Envelope) { e.Facts[1].Key = "a." + strings.Repeat("b", maxKeyLen) }, "longer"},
		{"overlong value", func(e *Envelope) { e.Facts[1].Value = strings.Repeat("x", maxValueLen+1) }, "longer"},
		{"overlong scope", func(e *Envelope) { e.Facts[1].Scope = strings.Repeat("x", maxScopeLen+1) }, "scope"},
		{"unknown type", func(e *Envelope) { e.Facts[1].Type = model.ValueType("colour") }, "unknown type"},
		{"too many facts", func(e *Envelope) {
			e.Facts = nil
			for i := 0; i <= maxFacts; i++ {
				e.Facts = append(e.Facts, FactItem{Key: fmt.Sprintf("product.k%d", i), Value: "v"})
			}
		}, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := okEnvelope()
			tc.mut(&e)
			err := e.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid envelope: %v", err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("accepted %s", tc.name)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("error %q does not mention %q, so it will not tell the author what to fix", err, tc.want)
			}
		})
	}
}

// TestSameKeyInDifferentScopesIsAllowed keeps two instances of one product on
// a host from colliding. Collapsing them would make the second instance
// invisible rather than wrong, which is worse.
func TestSameKeyInDifferentScopesIsAllowed(t *testing.T) {
	e := Envelope{
		Schema: EnvelopeSchema, Collector: "example", Status: "ok",
		Facts: []FactItem{
			{Key: "product.version", Value: "4.2.1", Scope: "instance-a"},
			{Key: "product.version", Value: "5.0.0", Scope: "instance-b"},
		},
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("two scoped instances were rejected: %v", err)
	}
}

// TestFailedEnvelopeNeedsNoFacts lets a collector report that it could not
// answer. Forcing it to invent an empty fact list that validates would make
// "it failed" and "it found nothing" the same message.
func TestFailedEnvelopeNeedsNoFacts(t *testing.T) {
	e := Envelope{Schema: EnvelopeSchema, Collector: "example", Status: "failed", Error: "command not found"}
	if err := e.Validate(); err != nil {
		t.Fatalf("a failure report was rejected: %v", err)
	}
}

// TestExternalCollectorGetsNoInheritedEnvironment is the boundary that stops a
// contributed script from reading this process's secrets out of its own
// environment.
func TestExternalCollectorGetsNoInheritedEnvironment(t *testing.T) {
	t.Setenv("HEARSAY_TEST_SECRET", "do-not-leak")

	script := writeReporter(t, `
import json, os
facts = [{"key": "env.leaked", "value": os.environ.get("HEARSAY_TEST_SECRET", "")},
         {"key": "env.target", "value": os.environ.get("HEARSAY_TARGET", "")}]
print(json.dumps({"schema": os.environ["HEARSAY_SCHEMA"], "collector": "probe",
                  "status": "ok", "facts": facts}))
`)

	e, err := RunExternal(context.Background(), External{
		Name: "probe", Path: script.cmd, Args: script.args, Timeout: 30 * time.Second,
	}, "appnode1", "198.51.100.11")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range e.Facts {
		got[f.Key] = f.Value
	}
	if got["env.leaked"] != "" {
		t.Errorf("the collector read %q from this process's environment", got["env.leaked"])
	}
	if got["env.target"] != "appnode1" {
		t.Errorf("the collector was told the target is %q", got["env.target"])
	}
}

// TestExternalCollectorOutputIsRejectedWhole covers the path an operator will
// actually hit: a script that prints something close to an envelope.
func TestExternalCollectorOutputIsRejectedWhole(t *testing.T) {
	cases := map[string]string{
		"not json":      `print("everything is fine")`,
		"unknown field": `print('{"schema": "hearsay.facts/v1", "collector": "p", "status": "ok", "facts": [], "extra": 1}')`,
		"bad fact":      `print('{"schema": "hearsay.facts/v1", "collector": "p", "status": "ok", "facts": [{"key": "ok.one", "value": "1"}, {"key": "two", "value": "2"}]}')`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			script := writeReporter(t, body)
			_, err := RunExternal(context.Background(), External{
				Name: "probe", Path: script.cmd, Args: script.args, Timeout: 30 * time.Second,
			}, "appnode1", "198.51.100.11")
			if err == nil {
				t.Fatal("accepted output that is not a valid envelope")
			}
			if !strings.Contains(err.Error(), "probe") {
				t.Errorf("error %q does not name the collector at fault", err)
			}
		})
	}
}

// TestExternalCollectorThatHangsIsGivenUpOn. A collector is operator-supplied
// code; it must not be able to stop a collection run by never returning.
func TestExternalCollectorThatHangsIsGivenUpOn(t *testing.T) {
	script := writeReporter(t, "import time\ntime.sleep(30)\n")
	start := time.Now()
	_, err := RunExternal(context.Background(), External{
		Name: "slow", Path: script.cmd, Args: script.args, Timeout: 1 * time.Second,
	}, "appnode1", "198.51.100.11")
	if err == nil {
		t.Fatal("waited for a collector that never answered")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("error %q does not say the collector timed out", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the timeout took %s to take effect", elapsed)
	}
}

// FuzzEnvelope checks that no byte sequence makes decoding an envelope panic.
// External collector output is attacker-adjacent: anyone who can edit a script
// on the collecting host, or who controls what a remote command prints, chooses
// these bytes.
func FuzzEnvelope(f *testing.F) {
	ok, _ := json.Marshal(okEnvelope())
	f.Add(string(ok))
	f.Add(`{"schema":"hearsay.facts/v1","collector":"c","status":"ok","facts":null}`)
	f.Add(`{"facts":[{"key":"","value":"","type":"","scope":""}]}`)
	f.Add("")
	f.Add("{")

	f.Fuzz(func(t *testing.T, in string) {
		var e Envelope
		if err := json.Unmarshal([]byte(in), &e); err != nil {
			return
		}
		if err := e.Validate(); err == nil && e.Status == statusOK {
			for _, fact := range e.Facts {
				if !keyPattern.MatchString(fact.Key) {
					t.Fatalf("accepted key %q", fact.Key)
				}
			}
		}
	})
}

type reporter struct {
	cmd  string
	args []string
}

// writeReporter puts a small Python program on disk. Python is used because it
// is the language an operator is most likely to write a collector in, so the
// test exercises the same path they will.
func writeReporter(t *testing.T, body string) reporter {
	t.Helper()
	python := lookPython(t)
	path := filepath.Join(t.TempDir(), "collector.py")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return reporter{cmd: python, args: []string{path}}
}

func lookPython(t *testing.T) string {
	t.Helper()
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "python3"}
	}
	for _, n := range names {
		p, err := exec.LookPath(n)
		if err != nil {
			continue
		}
		// Windows ships an App Execution Alias named python.exe that only
		// opens the Store. It is on PATH and exits 9009, so finding the file
		// is not proof there is an interpreter behind it.
		if exec.Command(p, "-c", "pass").Run() == nil {
			return p
		}
	}
	t.Skip("no working python on PATH; this test needs a real program to run")
	return ""
}
