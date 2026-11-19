package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

const sampleYAML = `defaults:
  transport: ssh
  user: ops
  credential: keyring:kryostate/ops
  port: 22
stacks:
  - name: payments-prod
    owner: team-payments
    environment: prod
hosts:
  - name: web01
    fqdn: web01.example.com
    application: ledger
    stacks:
      - payments-prod:app
      - {stack: edge, role: proxy}
    target:
      user: svc-ro
      address: 10.0.0.5
      bastion: "ops@jump.example.com:22/agent:"
  - name: db01
    stacks: [payments-prod:database]
  - name: lab01
    target: false
`

const sampleJSON = `{
	"defaults": {"transport": "ssh", "user": "ops", "credential": "keyring:kryostate/ops", "port": 22},
	"stacks": [{"name": "payments-prod", "owner": "team-payments", "environment": "prod"}],
	"hosts": [
		{"name": "web01", "fqdn": "web01.example.com", "application": "ledger",
		 "stacks": ["payments-prod:app", {"stack": "edge", "role": "proxy"}],
		 "target": {"user": "svc-ro", "address": "10.0.0.5", "bastion": "ops@jump.example.com:22/agent:"}},
		{"name": "db01", "stacks": ["payments-prod:database"]},
		{"name": "lab01", "target": false}
	]
}`

func mustLoad(t *testing.T, src, format string) *File {
	t.Helper()
	f, err := Load([]byte(src), format)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func withoutLines(f *File) *File {
	for i := range f.Hosts {
		f.Hosts[i].Line = 0
	}
	for i := range f.Stacks {
		f.Stacks[i].Line = 0
	}
	return f
}

func TestYAMLAndJSONReadTheSame(t *testing.T) {
	y := withoutLines(mustLoad(t, sampleYAML, FormatYAML))
	j := withoutLines(mustLoad(t, sampleJSON, FormatJSON))
	if !reflect.DeepEqual(y, j) {
		t.Fatalf("yaml and json differ:\n%+v\n%+v", y, j)
	}
	if got := y.Hosts[0].Stacks; !reflect.DeepEqual(got, []Membership{{"payments-prod", "app"}, {"edge", "proxy"}}) {
		t.Fatalf("memberships: %+v", got)
	}
	if DetectFormat("-", []byte(" \n{\"hosts\": []}")) != FormatJSON || DetectFormat("x.yml", []byte("{")) != FormatYAML {
		t.Fatal("format detection")
	}
}

func TestDefaultsMergeUnderEachHost(t *testing.T) {
	f := mustLoad(t, sampleYAML, FormatYAML)
	web, ok := f.Effective(f.Hosts[0])
	want := Target{Transport: "ssh", Address: "10.0.0.5", Port: 22, User: "svc-ro",
		Credential: "keyring:kryostate/ops", Bastion: "ops@jump.example.com:22/agent:"}
	if !ok || web != want {
		t.Fatalf("web01 target %+v, want %+v", web, want)
	}
	db, ok := f.Effective(f.Hosts[1])
	if !ok || db.User != "ops" || db.Credential != "keyring:kryostate/ops" || db.Address != "" {
		t.Fatalf("db01 target %+v", db)
	}
	if _, ok := f.Effective(f.Hosts[2]); ok {
		t.Fatal("target: false still gives a target")
	}
	bare := mustLoad(t, "hosts:\n  - name: web01\n", FormatYAML)
	if _, ok := bare.Effective(bare.Hosts[0]); ok {
		t.Fatal("a host with no target and no defaults gets one")
	}
}

func TestUnknownKeysAreErrorsWithTheirLine(t *testing.T) {
	src := "hosts:\n  - name: web01\n    target:\n      usr: ops\n    owner: x\n"
	_, err := Load([]byte(src), FormatYAML)
	var ps Problems
	// The misspelt user also leaves the target with no user, which is said
	// too.
	if !errors.As(err, &ps) || len(ps) != 3 {
		t.Fatalf("want three problems, got %v", err)
	}
	if ps[1].Line != 4 || !strings.Contains(ps[1].Msg, `unknown key "usr"`) || ps[1].Host != "web01" {
		t.Fatalf("unknown key in target: %+v", ps[1])
	}
	if ps[2].Line != 5 || !strings.Contains(ps[2].Msg, `unknown key "owner"`) {
		t.Fatalf("unknown key in host: %+v", ps[2])
	}
	_, err = Load([]byte(`{"hosts": [{"name": "web01", "fqnd": "web01.example.com"}]}`), FormatJSON)
	if !errors.As(err, &ps) || !strings.Contains(ps[0].Msg, `unknown key "fqnd"`) {
		t.Fatalf("json unknown key: %v", err)
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	src := `defaults:
  transport: telnet
hosts:
  - fqdn: nameless.example.com
  - name: web01
    target: {transport: ssh}
  - name: WEB01
    stacks: [""]
    target: {user: ops, port: 70000, credential: hunter2}
`
	_, err := Load([]byte(src), FormatYAML)
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("want problems, got %v", err)
	}
	wants := []string{"line 2: transport \"telnet\"", "line 4: a host needs a name", "line 4: target: no user", "line 5: host web01: target: no user",
		"line 7: host WEB01: listed twice", "line 8: host WEB01: a stack membership needs", "line 9: host WEB01: port",
		"line 9: host WEB01: credential is not a reference"}
	if len(ps) != len(wants) {
		t.Fatalf("want %d problems, got %d:\n%v", len(wants), len(ps), err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}
func TestPasswordsAreRefused(t *testing.T) {
	for _, src := range []string{
		"hosts:\n  - name: web01\n    target: {user: ops, credential: hunter2}\n",
		"hosts:\n  - name: web01\n    target: {user: ops, credential: 'pw:hunter2'}\n",
		"hosts:\n  - name: web01\n    target: {user: ops, password: hunter2}\n",
		"defaults: {user: ops, credential: hunter2}\nhosts:\n  - name: web01\n",
	} {
		_, err := Load([]byte(src), FormatYAML)
		if err == nil || !strings.Contains(err.Error(), "password") {
			t.Errorf("accepted %q: %v", src, err)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the password is echoed back: %v", err)
		}
	}
	for _, ref := range []string{"agent:", "keyring:kryostate/ops", "env:OPS_KEY", "file:/keys/ops.age", "exec:vault-read ops"} {
		if err := CheckCredentialRef(ref); err != nil {
			t.Errorf("%s refused: %v", ref, err)
		}
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := mustLoad(t, sampleYAML, FormatYAML)
	p, err := PlanImport(ctx, st, f, Options{By: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Counts(); c.HostsCreated != 3 || c.TargetsCreated != 2 || c.MembershipsAdded != 3 || c.StacksCreated != 2 {
		t.Fatalf("plan counts %+v", c)
	}
	hosts, _ := st.Hosts(ctx, store.HostFilter{})
	stacks, _ := st.Stacks(ctx)
	if len(hosts) != 0 || len(stacks) != 0 {
		t.Fatalf("planning wrote %d hosts, %d stacks", len(hosts), len(stacks))
	}
}

func TestApplyThenApplyAgainChangesNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := mustLoad(t, sampleYAML, FormatYAML)
	opt := Options{By: "ops", Source: "hosts.yaml"}
	p, err := PlanImport(ctx, st, f, opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, st, p, opt); err != nil {
		t.Fatal(err)
	}

	web, err := st.LookupCI(ctx, "web01.example.com")
	if err != nil {
		t.Fatal(err)
	}
	profiles, _ := st.ConnProfiles(ctx, web)
	if len(profiles) != 1 || profiles[0].Username != "svc-ro" || profiles[0].Address != "10.0.0.5" ||
		profiles[0].CredentialRef != "keyring:kryostate/ops" || profiles[0].BastionRef != "ops@jump.example.com:22/agent:" {
		t.Fatalf("web01 profiles %+v", profiles)
	}
	ms, _ := st.MembershipsOf(ctx, web)
	if !reflect.DeepEqual(ms, []store.Membership{{Stack: "edge", Role: "proxy"}, {Stack: "payments-prod", Role: "app"}}) {
		t.Fatalf("web01 memberships %+v", ms)
	}
	rows, _ := st.Hosts(ctx, store.HostFilter{Search: "web01"})
	if len(rows) != 1 || rows[0].Values[model.AttrApplication] != "ledger" {
		t.Fatalf("web01 application: %+v", rows)
	}
	db, _ := st.LookupCI(ctx, "db01")
	if ps, _ := st.ConnProfiles(ctx, db); len(ps) != 1 || ps[0].Username != "ops" || ps[0].Address != "db01" {
		t.Fatalf("db01 profiles %+v", ps)
	}
	lab, _ := st.LookupCI(ctx, "lab01")
	if ps, _ := st.ConnProfiles(ctx, lab); len(ps) != 0 {
		t.Fatalf("lab01 has target: false but got %+v", ps)
	}
	stk, err := st.Stack(ctx, "payments-prod")
	if err != nil || stk.Stack.Owner != "team-payments" || stk.Health.Members != 2 {
		t.Fatalf("stack %+v %v", stk, err)
	}

	again, err := PlanImport(ctx, st, f, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		var b strings.Builder
		again.Write(&b)
		t.Fatalf("second import would change:\n%s", b.String())
	}

	// A stack named without a role keeps the recorded role.
	bare := mustLoad(t, "defaults: {user: ops, credential: keyring:kryostate/ops, port: 22}\nhosts:\n  - name: db01\n    stacks: [payments-prod]\n", FormatYAML)
	if p, err := PlanImport(ctx, st, bare, opt); err != nil || !p.Empty() {
		t.Fatalf("naming the stack without its role changes something: %v %+v", err, p)
	}
}

func TestUpdateKeepsThePinnedHostKey(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	id, _, err := st.ResolveCI(ctx, model.KindHost, "web01", []model.Identity{{Scheme: model.IdentHostname, Value: "web01"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh", Address: "web01",
		Username: "old", CredentialRef: "agent:", Enabled: true, HostKey: "ssh-ed25519 AAAA"}); err != nil {
		t.Fatal(err)
	}
	f := mustLoad(t, "hosts:\n  - name: web01\n    fqdn: web01.example.com\n    target: {user: ops}\n", FormatYAML)
	p, err := PlanImport(ctx, st, f, Options{By: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hosts[0].Action != Update || p.Hosts[0].Target.Action != Update {
		t.Fatalf("plan %+v", p.Hosts[0])
	}
	if err := Apply(ctx, st, p, Options{By: "ops"}); err != nil {
		t.Fatal(err)
	}
	ps, _ := st.ConnProfiles(ctx, id)
	if len(ps) != 1 || ps[0].Username != "ops" || ps[0].HostKey != "ssh-ed25519 AAAA" || ps[0].Address != "web01" {
		t.Fatalf("profile after update %+v", ps)
	}
	if got, err := st.LookupCI(ctx, "web01.example.com"); err != nil || got != id {
		t.Fatalf("fqdn not added to the existing host: %v %v", got, err)
	}
}

func TestPlanProblemsWriteNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	for _, n := range []string{"web01", "web02"} {
		if _, _, err := st.ResolveCI(ctx, model.KindHost, n, []model.Identity{{Scheme: model.IdentHostname, Value: n}}); err != nil {
			t.Fatal(err)
		}
	}
	web02, _ := st.LookupCI(ctx, "web02")
	if err := st.AddIdentity(ctx, web02, model.IdentFQDN, "web01.example.com"); err != nil {
		t.Fatal(err)
	}
	f := mustLoad(t, "hosts:\n  - name: db01\n  - name: web01\n    fqdn: web01.example.com\n", FormatYAML)
	_, err := PlanImport(ctx, st, f, Options{})
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 1 || ps[0].Host != "web01" {
		t.Fatalf("want one problem for web01, got %v", err)
	}
	if _, err := st.LookupCI(ctx, "db01"); !errors.Is(err, store.ErrNoSuchHost) {
		t.Fatalf("db01 was written despite the problem: %v", err)
	}
}

func TestMissingKeyringCredentialIsAWarning(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := mustLoad(t, sampleYAML, FormatYAML)
	p, err := PlanImport(ctx, st, f, Options{CredentialMissing: func(ref string) bool { return ref == "keyring:kryostate/ops" }})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "kryostate secret set kryostate/ops") {
		t.Fatalf("warnings %q", p.Warnings)
	}
}

func TestFromStoreReadsBackUnchanged(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	f := mustLoad(t, sampleYAML, FormatYAML)
	p, err := PlanImport(ctx, st, f, Options{By: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, st, p, Options{By: "ops"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := FromStore(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{FormatYAML, FormatJSON} {
		b, err := Encode(out, format)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Load(b, format)
		if err != nil {
			t.Fatalf("%s export does not load: %v\n%s", format, err, b)
		}
		again, err := PlanImport(ctx, st, back, Options{By: "ops"})
		if err != nil {
			t.Fatal(err)
		}
		if !again.Empty() {
			var w strings.Builder
			again.Write(&w)
			t.Fatalf("%s export would change the store:\n%s\n%s", format, w.String(), b)
		}
	}
	if _, err := Load([]byte(Example), FormatYAML); err != nil {
		t.Fatalf("the example does not load: %v", err)
	}
}
