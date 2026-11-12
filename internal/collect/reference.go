package collect

// Reference material for people writing collectors of their own: an example
// value for what each collector records, and every catalog entry written out
// as it would appear in a collectors.json file. The definitions are built from
// the same Spec structs LoadSpecs parses, so what is shown is what loads.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Prefixes of the collector ids used in links and on the command line. A
// catalog entry is named by its own id, such as cpu.cores; the others carry a
// prefix no catalog id starts with, so the four kinds never collide.
const (
	RefBuiltin  = "builtin."  // builtin.os.version: the always-on specs for one attribute
	RefSpec     = "spec."     // spec.example.nodename: one spec from collectors.json
	RefExternal = "external." // external.vmware: one program from externals.json
)

// examples are plausible values for what each catalog entry records.
var examples = map[string]string{
	"cpu.cores":                 "8",
	"memory.total":              "15843",
	"timezone":                  "UTC",
	"dns.servers":               "10.0.0.2,10.0.0.3",
	"selinux":                   "Enforcing",
	"splunk.uf.version":         "9.2.1",
	"splunk.enterprise.version": "9.2.1",
	"splunk.home":               "/opt/splunkforwarder",
	"java.version":              "17.0.10",
	"python.version":            "3.11.7",
	"openssl.version":           "3.0.7",
	"nginx.version":             "1.24.0",
	"httpd.version":             "2.4.57",
	"docker.version":            "24.0.7",
	"package.version":           "3.0.7-27.el9",
	"file.exists":               "true",
	"file.contents":             "nameserver 10.0.0.2",
	"config.value":              "db01",
	"env.var":                   "/usr/lib/jvm/java-17",
}

// attrExamples are plausible values for what the always-on specs record.
var attrExamples = map[string]string{
	"os.family":       "Linux",
	"os.version":      "9.3",
	"os.kernel":       "5.14.0-362.el9.x86_64",
	"os.build":        "20348",
	"os.arch":         "x86_64",
	"os.bos_level":    "7.3.1.0",
	"host.wpar":       "0",
	"host.machine_id": "4c4c4544-0042-3510-8048-b4c04f4b3732",
	"hw.model":        "PowerEdge R650",
	"hw.serial":       "4B5X0Q3",
}

// builtinInfo names what the always-on specs record, for people.
var builtinInfo = map[string][2]string{
	"os.family":       {"OS family", "Which operating system the host runs. It decides which other commands are sent to it."},
	"os.version":      {"OS version", "The operating system release, such as 9.3 on Linux or 7300-01-02-2320 on AIX."},
	"os.kernel":       {"Kernel", "The running kernel release."},
	"os.build":        {"Windows build", "The Windows build number."},
	"os.arch":         {"Architecture", "The processor architecture the operating system is built for."},
	"os.bos_level":    {"AIX base OS level", "The level of the bos.rte fileset."},
	"host.wpar":       {"AIX workload partition", "Non-zero when the host is a WPAR rather than the machine hosting it."},
	"host.machine_id": {"Machine id", "Identity evidence: the operating system's own id for this installation."},
	"hw.model":        {"Hardware model", "The machine model the firmware reports."},
	"hw.serial":       {"Serial number", "The machine's serial number."},
}

// BuiltinTitle names what an always-on attribute is, or returns "" when it
// has no name of its own.
func BuiltinTitle(attr string) (title, description string) {
	i := builtinInfo[attr]
	return i[0], i[1]
}

// Example is a value the entry might record, for showing what to expect.
func (t Template) Example() string { return examples[t.ID] }

// AttrExample is a value an always-on attribute might hold, or blank.
func AttrExample(attr string) string { return attrExamples[attr] }

// ExampleParam is the value a template is shown with when there is no real
// one: its placeholder, and for a config setting the example key and section.
func (t Template) ExampleParam() string {
	if t.Param == nil {
		return ""
	}
	if t.Param.Config() {
		return ConfigParam(t.Param.Placeholder, t.Param.SectionPlaceholder, t.Param.KeyPlaceholder)
	}
	return t.Param.Placeholder
}

// Definition writes the entry out as the specs a person would put in their
// own collectors.json to collect the same thing, for the given value (blank
// for the example value). The ids are the entry's own, without the catalog.
// prefix the switched-on versions carry, so a copy pasted into a file does not
// clash with the catalog.
func (t Template) Definition(param string) ([]Spec, error) {
	if param == "" {
		param = t.ExampleParam()
	}
	specs, err := Choice{Template: t.ID, Param: param}.Specs()
	if err != nil {
		return nil, err
	}
	comment := t.Title
	if t.Description != "" {
		comment += ". " + t.Description
	}
	if !t.Fixed() {
		comment = t.Title + " (" + ParamText(t, param) + ")"
	}
	for i := range specs {
		specs[i].ID = t.ID + "." + t.Variants[i].Name
		specs[i].Comment = comment
		specs[i].Stacks = nil
		specs[i].Where = ""
	}
	return specs, nil
}

// SpecsJSON writes specs as a complete collectors.json file. It is indented
// the way the shipped file and the guide are, with short lists and parse
// rules kept on one line.
func SpecsJSON(specs []Spec) ([]byte, error) {
	if specs == nil {
		specs = []Spec{}
	}
	return PrettyJSON(specFile{Version: 1, Specs: specs})
}

// PrettyJSON marshals v with two-space indentation, keeping any list or object
// below the second level on one line when it is short, and without escaping
// <, > and &, which appear in commands such as 2>&1.
func PrettyJSON(v any) ([]byte, error) {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(&raw)
	dec.UseNumber()
	n, err := readNode(dec)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	n.write(&out, "", 0)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// node is a JSON value with its object keys kept in order.
type node struct {
	delim  json.Delim // '{' or '[' for a container, 0 for a scalar
	scalar string
	keys   []string
	vals   []*node
}

func readNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok := tok.(type) {
	case json.Delim:
		n := &node{delim: tok}
		for dec.More() {
			if tok == '{' {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k.(string))
			}
			v, err := readNode(dec)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return nil, err
		}
		return n, nil
	default:
		return &node{scalar: scalarJSON(tok)}, nil
	}
}

func scalarJSON(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case json.Number:
		return v.String()
	default:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
		return strings.TrimSuffix(b.String(), "\n")
	}
}

// inline is the value on one line, with a space after each comma and colon.
func (n *node) inline() string {
	if n.delim == 0 {
		return n.scalar
	}
	var parts []string
	for i, v := range n.vals {
		if n.delim == '{' {
			parts = append(parts, scalarJSON(n.keys[i])+": "+v.inline())
		} else {
			parts = append(parts, v.inline())
		}
	}
	if n.delim == '{' {
		if len(parts) == 0 {
			return "{}"
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (n *node) write(w io.Writer, indent string, depth int) {
	if n.delim == 0 {
		io.WriteString(w, n.scalar)
		return
	}
	if one := n.inline(); len(n.vals) == 0 || (depth >= 3 && len(indent)+len(one) <= 90) {
		io.WriteString(w, one)
		return
	}
	closer := "]"
	if n.delim == '{' {
		closer = "}"
	}
	fmt.Fprintf(w, "%c\n", rune(n.delim))
	inner := indent + "  "
	for i, v := range n.vals {
		io.WriteString(w, inner)
		if n.delim == '{' {
			io.WriteString(w, scalarJSON(n.keys[i])+": ")
		}
		v.write(w, inner, depth+1)
		if i < len(n.vals)-1 {
			io.WriteString(w, ",")
		}
		io.WriteString(w, "\n")
	}
	io.WriteString(w, indent+closer)
}

// BuiltinRefs groups the always-on specs by the attribute they record, in
// the order the attributes first appear, so os.version on Linux, AIX and
// Windows is one collector with three commands.
func BuiltinRefs() (order []string, byAttr map[string][]Spec, err error) {
	set, err := BuiltinSpecs()
	if err != nil {
		return nil, nil, err
	}
	byAttr = map[string][]Spec{}
	for _, s := range set.Specs {
		if _, ok := byAttr[s.Attr]; !ok {
			order = append(order, s.Attr)
		}
		byAttr[s.Attr] = append(byAttr[s.Attr], s)
	}
	return order, byAttr, nil
}

// DescribeParse says in a few words what a parse rule keeps.
func DescribeParse(p Parse) string {
	switch p.Kind {
	case ParseFirstLine, "":
		return "the first line"
	case ParseRegex:
		return "what the pattern " + p.Pattern + " captures"
	case ParseKV:
		return "the value of " + p.Key + "=…"
	case ParseColonKV:
		return "the value after \"" + p.Key + " :\""
	case ParseField:
		return fmt.Sprintf("field %d of the line containing %q", p.Field, p.Marker)
	case ParseJSONPath:
		return "the JSON value at " + p.Path
	case ParseAll:
		return "the whole output"
	}
	return string(p.Kind)
}
