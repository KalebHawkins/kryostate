// Package ingest reads hosts in bulk: an import file that declares hosts, the
// connections they are collected through and the stacks they belong to, and
// the plain host lists pasted into a stack.
package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// File is an import file: every host it lists, how each is reached, and the
// stacks it belongs to. YAML and JSON files have the same structure.
type File struct {
	// Defaults apply to every host's target unless the host overrides them.
	Defaults *Target `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	// Stacks carry stack details. A stack a host names that is not listed
	// here is created bare.
	Stacks []Stack `yaml:"stacks,omitempty" json:"stacks,omitempty"`
	Hosts  []Host  `yaml:"hosts" json:"hosts"`
}

// Target is how a host is reached. In defaults, address and disabled are not
// allowed: an address names one host.
type Target struct {
	Transport  string `yaml:"transport,omitempty" json:"transport,omitempty"`
	Address    string `yaml:"address,omitempty" json:"address,omitempty"`
	Port       int    `yaml:"port,omitempty" json:"port,omitempty"`
	User       string `yaml:"user,omitempty" json:"user,omitempty"`
	Credential string `yaml:"credential,omitempty" json:"credential,omitempty"`
	Bastion    string `yaml:"bastion,omitempty" json:"bastion,omitempty"`
	// Disabled records the connection without ever collecting through it.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Stack is a stack's details.
type Stack struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Owner       string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Environment string `yaml:"environment,omitempty" json:"environment,omitempty"`
	Line        int    `yaml:"-" json:"-"`
}

// Host is one host in the file.
type Host struct {
	Name        string       `yaml:"name" json:"name"`
	FQDN        string       `yaml:"fqdn,omitempty" json:"fqdn,omitempty"`
	Application string       `yaml:"application,omitempty" json:"application,omitempty"`
	Stacks      []Membership `yaml:"stacks,omitempty" json:"stacks,omitempty"`
	// Target is nil when the file does not say, which means the defaults,
	// if there are any.
	Target *HostTarget `yaml:"target,omitempty" json:"target,omitempty"`
	// Disabled is nil when the file does not say, which leaves the host as
	// it is.
	Disabled *bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	Line     int   `yaml:"-" json:"-"`
}

// HostTarget is a host's target: its own fields over the defaults, or None
// for "target: false", a host with no connection.
type HostTarget struct {
	None bool
	Target
}

// Membership is a stack a host belongs to, with its role there. In a file it
// is "stack", "stack:role" or {stack: ..., role: ...}.
type Membership struct {
	Stack string `yaml:"stack" json:"stack"`
	Role  string `yaml:"role,omitempty" json:"role,omitempty"`
}

// Problem is one thing wrong with a file.
type Problem struct {
	Line int
	Host string
	Msg  string
}

func (p Problem) String() string {
	var b strings.Builder
	if p.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", p.Line)
	}
	if p.Host != "" {
		fmt.Fprintf(&b, "host %s: ", p.Host)
	}
	b.WriteString(p.Msg)
	return b.String()
}

// Problems is every problem found in a file. Nothing is written while there
// are any.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = "  " + p.String()
	}
	noun := "problems"
	if len(ps) == 1 {
		noun = "problem"
	}
	return fmt.Sprintf("%d %s, nothing was written:\n%s", len(ps), noun, strings.Join(lines, "\n"))
}

// Formats.
const (
	FormatYAML = "yaml"
	FormatJSON = "json"
)

// DetectFormat picks a file's format from its extension, else its content: a
// file starting with { is JSON.
func DetectFormat(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return FormatJSON
	case ".yaml", ".yml":
		return FormatYAML
	}
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '{' {
		return FormatJSON
	}
	return FormatYAML
}

// Load reads and checks a whole file. Every problem is reported together,
// each with its line, so one pass fixes them all.
func Load(data []byte, format string) (*File, error) {
	switch format {
	case FormatJSON:
		if !json.Valid(data) {
			var v any
			err := json.Unmarshal(data, &v)
			var se *json.SyntaxError
			if errors.As(err, &se) {
				return nil, Problems{{Line: lineAt(data, se.Offset), Msg: "not valid JSON: " + se.Error()}}
			}
			return nil, Problems{{Msg: fmt.Sprintf("not valid JSON: %v", err)}}
		}
		// Valid JSON is YAML, so both go through one decoder and report the
		// same problems on the same lines. A tab outside a string is only
		// whitespace in valid JSON, and the YAML scanner is fussy about them.
		data = bytes.ReplaceAll(data, []byte("\t"), []byte(" "))
	case FormatYAML:
	default:
		return nil, fmt.Errorf("unknown format %q: use yaml or json", format)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, Problems{{Msg: err.Error()}}
	}
	d := &decoder{}
	f := &File{}
	if len(doc.Content) == 0 {
		return nil, Problems{{Msg: "the file is empty; see kryostate import --example"}}
	}
	d.file(doc.Content[0], f)
	d.problems = append(d.problems, f.validate()...)
	if len(d.problems) > 0 {
		sort.SliceStable(d.problems, func(i, j int) bool { return d.problems[i].Line < d.problems[j].Line })
		return nil, d.problems
	}
	return f, nil
}

// decoder turns the YAML tree into a File, refusing keys it does not know
// so a misspelt one is not silently ignored.
type decoder struct {
	problems Problems
	host     string
}

func (d *decoder) fail(n *yaml.Node, format string, args ...any) {
	d.problems = append(d.problems, Problem{Line: n.Line, Host: d.host, Msg: fmt.Sprintf(format, args...)})
}

// fields walks a mapping, calling set for each known key and reporting the
// rest.
func (d *decoder) fields(n *yaml.Node, where string, known []string, set func(key string, v *yaml.Node)) {
	if n.Kind != yaml.MappingNode {
		d.fail(n, "%s should be a mapping of %s", where, strings.Join(known, ", "))
		return
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		switch {
		case seen[k.Value]:
			d.fail(k, "%s: %s given twice", where, k.Value)
			continue
		case !contains(known, k.Value):
			if isPasswordKey(k.Value) {
				d.fail(k, "%s: %q is not accepted: a password never goes in an import file. Give credential: keyring:kryostate/<user> and store the password with kryostate secret set kryostate/<user>", where, k.Value)
			} else {
				d.fail(k, "%s: unknown key %q (known: %s)", where, k.Value, strings.Join(known, ", "))
			}
			continue
		}
		seen[k.Value] = true
		set(k.Value, v)
	}
}

func isPasswordKey(k string) bool {
	switch strings.ToLower(k) {
	case "password", "pass", "passwd", "secret", "pwd":
		return true
	}
	return false
}

func (d *decoder) str(n *yaml.Node, key string) string {
	if n.Kind != yaml.ScalarNode {
		d.fail(n, "%s should be a string", key)
		return ""
	}
	if n.Tag == "!!null" {
		return ""
	}
	return strings.TrimSpace(n.Value)
}

func (d *decoder) boolean(n *yaml.Node, key string) bool {
	var b bool
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" || n.Decode(&b) != nil {
		d.fail(n, "%s should be true or false", key)
	}
	return b
}

func (d *decoder) file(n *yaml.Node, f *File) {
	d.fields(n, "the file", []string{"defaults", "stacks", "hosts"}, func(key string, v *yaml.Node) {
		switch key {
		case "defaults":
			t := Target{}
			d.target(v, "defaults", &t, true)
			f.Defaults = &t
		case "stacks":
			d.list(v, "stacks", func(item *yaml.Node) {
				s := Stack{Line: item.Line}
				d.fields(item, "stack", []string{"name", "description", "owner", "environment"}, func(key string, v *yaml.Node) {
					switch key {
					case "name":
						s.Name = d.str(v, key)
					case "description":
						s.Description = d.str(v, key)
					case "owner":
						s.Owner = d.str(v, key)
					case "environment":
						s.Environment = d.str(v, key)
					}
				})
				f.Stacks = append(f.Stacks, s)
			})
		case "hosts":
			d.list(v, "hosts", func(item *yaml.Node) {
				f.Hosts = append(f.Hosts, d.hostNode(item))
			})
		}
	})
}

func (d *decoder) list(n *yaml.Node, key string, each func(*yaml.Node)) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return
	}
	if n.Kind != yaml.SequenceNode {
		d.fail(n, "%s should be a list", key)
		return
	}
	for _, item := range n.Content {
		each(item)
	}
}

func (d *decoder) hostNode(n *yaml.Node) Host {
	h := Host{Line: n.Line}
	// The name is read first, so every other problem in the host can say
	// which host it is in.
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "name" && n.Content[i+1].Kind == yaml.ScalarNode {
				d.host = strings.TrimSpace(n.Content[i+1].Value)
			}
		}
	}
	defer func() { d.host = "" }()
	known := []string{"name", "fqdn", "application", "stacks", "target", "disabled"}
	d.fields(n, "host", known, func(key string, v *yaml.Node) {
		switch key {
		case "name":
			h.Name = d.str(v, key)
		case "fqdn":
			h.FQDN = strings.ToLower(d.str(v, key))
		case "application":
			h.Application = d.str(v, key)
		case "disabled":
			b := d.boolean(v, key)
			h.Disabled = &b
		case "stacks":
			d.list(v, "stacks", func(item *yaml.Node) {
				if m, ok := d.membership(item); ok {
					h.Stacks = append(h.Stacks, m)
				}
			})
		case "target":
			ht := &HostTarget{}
			if v.Kind == yaml.ScalarNode {
				// target: false is a host with no connection; true is
				// the defaults alone.
				ht.None = !d.boolean(v, "target (false, true or a mapping)")
			} else {
				d.target(v, "target", &ht.Target, false)
			}
			h.Target = ht
		}
	})
	return h
}

func (d *decoder) membership(n *yaml.Node) (Membership, bool) {
	var m Membership
	switch n.Kind {
	case yaml.ScalarNode:
		s := d.str(n, "a stack")
		m.Stack, m.Role, _ = strings.Cut(s, ":")
	case yaml.MappingNode:
		d.fields(n, "stack membership", []string{"stack", "role"}, func(key string, v *yaml.Node) {
			switch key {
			case "stack":
				m.Stack = d.str(v, key)
			case "role":
				m.Role = d.str(v, key)
			}
		})
	default:
		d.fail(n, `a stack should be "stack", "stack:role" or {stack: ..., role: ...}`)
		return m, false
	}
	m.Stack, m.Role = strings.TrimSpace(m.Stack), strings.TrimSpace(m.Role)
	if m.Stack == "" {
		d.fail(n, "a stack membership needs the stack's name")
		return m, false
	}
	return m, true
}

func (d *decoder) target(n *yaml.Node, where string, t *Target, isDefaults bool) {
	known := []string{"transport", "address", "port", "user", "credential", "bastion", "disabled"}
	if isDefaults {
		known = []string{"transport", "port", "user", "credential", "bastion"}
	}
	d.fields(n, where, known, func(key string, v *yaml.Node) {
		switch key {
		case "transport":
			t.Transport = strings.ToLower(d.str(v, key))
			if t.Transport != "ssh" && t.Transport != "winrm" {
				d.fail(v, "transport %q should be ssh or winrm", t.Transport)
			}
		case "address":
			t.Address = d.str(v, key)
		case "user":
			t.User = d.str(v, key)
		case "credential":
			t.Credential = d.str(v, key)
			if err := CheckCredentialRef(t.Credential); err != nil {
				d.fail(v, "%v", err)
			}
		case "bastion":
			t.Bastion = d.str(v, key)
			if err := checkBastion(t.Bastion); err != nil {
				d.fail(v, "%v", err)
			}
		case "disabled":
			t.Disabled = d.boolean(v, key)
		case "port":
			p, err := strconv.Atoi(v.Value)
			if v.Kind != yaml.ScalarNode || v.Tag != "!!int" || err != nil || p < 1 || p > 65535 {
				d.fail(v, "port should be a number from 1 to 65535")
				return
			}
			t.Port = p
		}
	})
}

// Effective is the target a host gets: its own fields over the defaults. It
// is false when the host gets no connection.
func (f *File) Effective(h Host) (Target, bool) {
	var t Target
	if f.Defaults != nil {
		t = *f.Defaults
	}
	switch {
	case h.Target == nil && f.Defaults == nil:
		return Target{}, false
	case h.Target != nil && h.Target.None:
		return Target{}, false
	case h.Target != nil:
		o := h.Target.Target
		pick := func(dst *string, v string) {
			if v != "" {
				*dst = v
			}
		}
		pick(&t.Transport, o.Transport)
		pick(&t.Address, o.Address)
		pick(&t.User, o.User)
		pick(&t.Credential, o.Credential)
		pick(&t.Bastion, o.Bastion)
		if o.Port != 0 {
			t.Port = o.Port
		}
		t.Disabled = o.Disabled
	}
	if t.Transport == "" {
		t.Transport = "ssh"
	}
	if t.Credential == "" {
		t.Credential = "agent:"
	}
	return t, true
}

// validate checks what the structure alone cannot: names given and not
// repeated, and every target complete.
func (f *File) validate() Problems {
	var ps Problems
	add := func(line int, host, format string, args ...any) {
		ps = append(ps, Problem{Line: line, Host: host, Msg: fmt.Sprintf(format, args...)})
	}
	stacks := map[string]int{}
	for _, s := range f.Stacks {
		switch {
		case s.Name == "":
			add(s.Line, "", "a stack needs a name")
		case stacks[s.Name] != 0:
			add(s.Line, "", "stack %s is listed twice (also on line %d)", s.Name, stacks[s.Name])
		default:
			stacks[s.Name] = s.Line
		}
	}
	if len(f.Hosts) == 0 {
		add(0, "", "no hosts: the file needs a hosts: list; see kryostate import --example")
	}
	names, fqdns := map[string]int{}, map[string]int{}
	for _, h := range f.Hosts {
		if h.Name == "" {
			add(h.Line, "", "a host needs a name")
		} else if strings.ContainsAny(h.Name, " \t/") {
			add(h.Line, h.Name, "a host name cannot contain spaces or /")
		} else if l, dup := names[strings.ToLower(h.Name)]; dup {
			add(h.Line, h.Name, "listed twice (also on line %d)", l)
		} else {
			names[strings.ToLower(h.Name)] = h.Line
		}
		if h.FQDN != "" {
			if strings.ContainsAny(h.FQDN, " \t/") || !strings.Contains(h.FQDN, ".") {
				add(h.Line, h.Name, "fqdn %q is not a fully qualified name", h.FQDN)
			} else if l, dup := fqdns[h.FQDN]; dup {
				add(h.Line, h.Name, "fqdn %s is also given on line %d", h.FQDN, l)
			} else {
				fqdns[h.FQDN] = h.Line
			}
		}
		seen := map[string]bool{}
		for _, m := range h.Stacks {
			if seen[m.Stack] {
				add(h.Line, h.Name, "stack %s is named twice", m.Stack)
			}
			seen[m.Stack] = true
		}
		if t, ok := f.Effective(h); ok {
			if t.User == "" {
				add(h.Line, h.Name, "target: no user; give user in the target or in defaults")
			}
			if t.Bastion != "" && t.Transport == "winrm" {
				add(h.Line, h.Name, "target: a bastion is for ssh only")
			}
		}
	}
	return ps
}

// CheckCredentialRef accepts a reference to a credential and nothing else: a
// value that is not scheme:value in a known scheme may be a password typed
// where a reference belongs, so it is refused and not echoed.
func CheckCredentialRef(ref string) error {
	scheme, value, ok := strings.Cut(ref, ":")
	bad := func(why string) error {
		return fmt.Errorf("credential is not a reference (%s): give agent:, keyring:service/account, env:NAME, file:PATH or exec:COMMAND; a password never goes in an import file", why)
	}
	if !ok {
		return bad("no scheme")
	}
	switch scheme {
	case "agent":
		return nil
	case "keyring":
		svc, acct, ok := strings.Cut(value, "/")
		if !ok || svc == "" || acct == "" {
			return bad("keyring: needs service/account")
		}
	case "env", "file", "exec":
		if strings.TrimSpace(value) == "" {
			return bad(scheme + ": needs a value")
		}
		if scheme == "env" && strings.ContainsAny(value, " \t=") {
			return bad("env: needs a variable name")
		}
	default:
		return bad("unknown scheme")
	}
	return nil
}

// checkBastion reads a bastion the way the ssh transport does:
// user@host[:port]/credential-ref, the credential after the first /.
func checkBastion(b string) error {
	_, _, _, ref, err := collect.ParseBastion(b)
	if err != nil {
		return fmt.Errorf("bastion %q: %w", b, err)
	}
	if err := CheckCredentialRef(ref); err != nil {
		return fmt.Errorf("bastion: %w", err)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func lineAt(data []byte, offset int64) int {
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	return bytes.Count(data[:offset], []byte("\n")) + 1
}

// MarshalYAML writes false for a host with no connection.
func (t HostTarget) MarshalYAML() (any, error) {
	if t.None {
		return false, nil
	}
	return t.Target, nil
}

// MarshalJSON writes false for a host with no connection.
func (t HostTarget) MarshalJSON() ([]byte, error) {
	if t.None {
		return []byte("false"), nil
	}
	return json.Marshal(t.Target)
}

// MarshalYAML writes the short "stack:role" form where it reads back the
// same.
func (m Membership) MarshalYAML() (any, error) {
	if s, ok := m.short(); ok {
		return s, nil
	}
	type plain Membership
	return plain(m), nil
}

// MarshalJSON writes the short "stack:role" form where it reads back the
// same.
func (m Membership) MarshalJSON() ([]byte, error) {
	if s, ok := m.short(); ok {
		return json.Marshal(s)
	}
	type plain Membership
	return json.Marshal(plain(m))
}

func (m Membership) short() (string, bool) {
	if strings.Contains(m.Stack, ":") {
		return "", false
	}
	if m.Role == "" {
		return m.Stack, true
	}
	return m.Stack + ":" + m.Role, true
}
