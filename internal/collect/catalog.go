package collect

// The catalog is a library of collectors a person can switch on without
// writing a command or a parse rule.
//
// Two kinds of entry exist. A fixed entry, such as the CPU core count, runs a
// command written here and takes no input. A template, such as "does this file
// exist", takes one value from the person: a path, a variable name or a
// package name. That value is checked against a narrow pattern for its kind
// and then placed inside single quotes, where neither a POSIX shell nor
// PowerShell interprets anything, so it cannot become a different command.
//
// Every command here only reads. Entries are optional: a host without the
// program is not a failed collection.

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// ParamKind says what kind of value a template takes, and so how it is
// checked.
type ParamKind string

const (
	ParamPath    ParamKind = "path"
	ParamEnvVar  ParamKind = "envvar"
	ParamPackage ParamKind = "package"
	// ParamConfig is a file, a key, and optionally the [section] the key is
	// in. It is stored as one value, PATH#KEY or PATH#[SECTION]KEY, which
	// ConfigParam builds and SplitConfigParam takes apart. # cannot appear in
	// a path kryostate accepts, so the split is unambiguous.
	ParamConfig ParamKind = "config"
)

// Param describes the one value a template needs.
type Param struct {
	Kind        ParamKind
	Label       string
	Placeholder string
	Help        string
	// For ParamConfig, examples for the key and section fields.
	KeyPlaceholder     string
	SectionPlaceholder string
}

// Config reports whether the value is a file, key and section.
func (p Param) Config() bool { return p.Kind == ParamConfig }

// Variant is how one entry is read on some operating systems. The command may
// contain {{param}}, which is replaced by the quoted value.
type Variant struct {
	Name    string // suffix for the spec id, such as "unix" or "windows"
	OS      []string
	Command string
	Parse   Parse
}

// Template is one catalog entry.
type Template struct {
	ID          string
	Title       string
	Category    string
	Description string
	// Attr is the attribute a fixed entry records. A template derives one
	// from its value unless the person names one.
	Attr     string
	Type     model.ValueType
	Param    *Param
	Variants []Variant
	// Reveals marks an entry whose value is the content of something on the
	// host, such as a file or a variable. Such a value may be a secret, so
	// the web interface only offers these when the operator allows it.
	Reveals bool
}

// Fixed reports whether the entry takes no value.
func (t Template) Fixed() bool { return t.Param == nil }

// Systems lists the operating systems the entry has a command for.
func (t Template) Systems() []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range t.Variants {
		for _, o := range v.OS {
			if !seen[o] {
				seen[o] = true
				out = append(out, o)
			}
		}
	}
	return out
}

var unix = []string{"linux", "aix"}
var windows = []string{"windows"}
var linux = []string{"linux"}

func rx(p string) Parse { return Parse{Kind: ParseRegex, Pattern: p} }
func kv(k string) Parse { return Parse{Kind: ParseKV, Key: k} }
func first() Parse      { return Parse{Kind: ParseFirstLine} }
func everything() Parse { return Parse{Kind: ParseAll} }

// sbin puts the system program folders on the PATH. A login that is not root
// on Debian and its relatives has neither /usr/sbin nor /sbin, which is where
// nginx, getenforce and AIX's lsdev live.
func sbin(cmd string) string { return `PATH="$PATH:/usr/sbin:/sbin"; ` + cmd }

// configAwk prints the value of one key = value line, the first one found,
// optionally only within a [section]. It exits 1 when the key is not there,
// so a missing key reads as "nothing found" rather than as an empty value.
const configAwk = `awk -v s={{section}} -v k={{key}} '` +
	`/^[ \t]*\[.*\][ \t\r]*$/ { sec = $0; sub(/^[ \t]*\[/, "", sec); sub(/\][ \t\r]*$/, "", sec); next } ` +
	`{ i = index($0, "="); if (i < 2) next; n = substr($0, 1, i - 1); gsub(/^[ \t]+|[ \t]+$/, "", n); ` +
	`if (n == k && (s == "" || sec == s)) { v = substr($0, i + 1); gsub(/^[ \t]+|[ \t\r]+$/, "", v); print v; found = 1; exit } } ` +
	`END { if (!found) exit 1 }' {{path}}`

const configPowerShell = `$s = {{section}}; $k = {{key}}; $sec = ''; $found = $false; ` +
	`foreach ($l in Get-Content -LiteralPath {{path}}) { ` +
	`if ($l -match '^\s*\[(.*)\]\s*$') { $sec = $Matches[1]; continue }; ` +
	`$i = $l.IndexOf('='); if ($i -lt 1) { continue }; ` +
	`if ($l.Substring(0, $i).Trim() -eq $k -and ($s -eq '' -or $sec -eq $s)) { $l.Substring($i + 1).Trim(); $found = $true; break } }; ` +
	`if (-not $found) { exit 1 }`

// Catalog is every entry, in the order they are offered.
var Catalog = []Template{
	// ---- operating system and hardware ----
	{
		ID: "cpu.cores", Title: "CPU cores", Category: "OS & hardware", Attr: "hw.cpu.cores", Type: model.TypeNumber,
		Description: "Logical processors the operating system can use.",
		Variants: []Variant{
			{Name: "linux", OS: linux, Command: "getconf _NPROCESSORS_ONLN", Parse: first()},
			{Name: "aix", OS: []string{"aix"}, Command: sbin("lsdev -Cc processor -S Available | wc -l"), Parse: first()},
			{Name: "windows", OS: windows, Command: "(Get-CimInstance Win32_ComputerSystem).NumberOfLogicalProcessors", Parse: first()},
		},
	},
	{
		ID: "memory.total", Title: "Memory total (MB)", Category: "OS & hardware", Attr: "hw.memory.total_mb", Type: model.TypeNumber,
		Description: "Installed memory in megabytes. A size, not a usage figure.",
		Variants: []Variant{
			{Name: "linux", OS: linux, Command: `awk '/^MemTotal:/ {printf "%d\n", $2/1024}' /proc/meminfo`, Parse: first()},
			{Name: "aix", OS: []string{"aix"}, Command: sbin("prtconf -m"), Parse: rx(`([0-9]+)`)},
			{Name: "windows", OS: windows, Command: "[math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1MB)", Parse: first()},
		},
	},

	{
		ID: "timezone", Title: "Time zone", Category: "OS & hardware", Attr: "os.timezone", Type: model.TypeText,
		Description: "The time zone the host reports.",
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "date +%Z", Parse: first()},
			{Name: "windows", OS: windows, Command: "(Get-TimeZone).Id", Parse: first()},
		},
	},
	{
		ID: "dns.servers", Title: "DNS servers", Category: "OS & hardware", Attr: "net.dns.servers", Type: model.TypeList,
		Description: "The name servers the host is configured to ask, comma-separated.",
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "awk '/^nameserver/ {print $2}' /etc/resolv.conf | paste -s -d, -", Parse: first()},
			{Name: "windows", OS: windows, Command: "(Get-DnsClientServerAddress -AddressFamily IPv4 | Select-Object -ExpandProperty ServerAddresses | Select-Object -Unique) -join ','", Parse: first()},
		},
	},
	{
		ID: "selinux", Title: "SELinux mode", Category: "OS & hardware", Attr: "os.selinux", Type: model.TypeText,
		Description: "Enforcing, Permissive or Disabled.",
		Variants:    []Variant{{Name: "linux", OS: linux, Command: sbin("getenforce"), Parse: first()}},
	},

	// ---- splunk ----
	{
		ID: "splunk.uf.version", Title: "Splunk Universal Forwarder version", Category: "Splunk", Attr: "splunk.uf.version", Type: model.TypeText,
		Description: "Read from the forwarder's splunk.version file in its default location.",
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "cat /opt/splunkforwarder/etc/splunk.version", Parse: kv("VERSION")},
			{Name: "windows", OS: windows, Command: `Get-Content 'C:\Program Files\SplunkUniversalForwarder\etc\splunk.version'`, Parse: kv("VERSION")},
		},
	},
	{
		ID: "splunk.enterprise.version", Title: "Splunk Enterprise version", Category: "Splunk", Attr: "splunk.enterprise.version", Type: model.TypeText,
		Description: "Read from Splunk Enterprise's splunk.version file in its default location.",
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "cat /opt/splunk/etc/splunk.version", Parse: kv("VERSION")},
			{Name: "windows", OS: windows, Command: `Get-Content 'C:\Program Files\Splunk\etc\splunk.version'`, Parse: kv("VERSION")},
		},
	},
	{
		ID: "splunk.home", Title: "Splunk home (from the running splunkd)", Category: "Splunk", Attr: "splunk.home", Type: model.TypePath,
		Description: "Where Splunk is installed, read from the running splunkd process, so it is right even outside the default location.",
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "ps -eo args | grep '[s]plunkd'", Parse: rx(`(/[^ ]+)/bin/splunkd`)},
			{Name: "windows", OS: windows, Command: `(Get-CimInstance Win32_Service | Where-Object { $_.Name -like 'Splunk*' } | Select-Object -First 1).PathName`, Parse: rx(`^"?(.+?)\\bin\\splunkd`)},
		},
	},
	// ---- runtimes and packages ----
	{
		ID: "java.version", Title: "Java version", Category: "Runtimes & packages", Attr: "java.version", Type: model.TypeText,
		Description: "The java on the PATH of the collecting user.",
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "java -version 2>&1", Parse: rx(`version "([^"]+)"`)},
			{Name: "windows", OS: windows, Command: `cmd /c "java -version 2>&1"`, Parse: rx(`version "([^"]+)"`)},
		},
	},
	{
		ID: "python.version", Title: "Python version", Category: "Runtimes & packages", Attr: "python.version", Type: model.TypeText,
		Description: "python3 on Linux and AIX, python on Windows.",
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "python3 --version 2>&1", Parse: rx(`Python ([0-9][0-9.]*)`)},
			{Name: "windows", OS: windows, Command: `cmd /c "python --version 2>&1"`, Parse: rx(`Python ([0-9][0-9.]*)`)},
		},
	},
	{
		ID: "openssl.version", Title: "OpenSSL version", Category: "Runtimes & packages", Attr: "openssl.version", Type: model.TypeText,
		Description: "The openssl command on the PATH.",
		Variants:    []Variant{{Name: "unix", OS: unix, Command: "openssl version", Parse: rx(`OpenSSL ([0-9][^ ]*)`)}},
	},
	{
		ID: "docker.version", Title: "Docker version", Category: "Runtimes & packages", Attr: "docker.version", Type: model.TypeText,
		Description: "The docker client installed on the host.",
		Variants:    []Variant{{Name: "linux", OS: linux, Command: "docker --version", Parse: rx(`version ([0-9][0-9.]*)`)}},
	},
	{
		ID: "package.version", Title: "Installed package version", Category: "Runtimes & packages", Type: model.TypeText,
		Description: "Any package by name: rpm or dpkg on Linux, lslpp on AIX, the installed programs list on Windows.",
		Param:       &Param{Kind: ParamPackage, Label: "Package", Placeholder: "openssl", Help: "The package name. On Windows, the start of the program's name, such as 'Splunk'."},
		Variants: []Variant{
			{Name: "linux", OS: linux, Command: `rpm -q --qf '%{VERSION}-%{RELEASE}\n' {{param}} 2>/dev/null || dpkg-query -W -f='${Version}\n' {{param}}`, Parse: first()},
			{Name: "aix", OS: []string{"aix"}, Command: "lslpp -Lqc {{param}}", Parse: rx(`^[^:]*:[^:]*:([^:]+)`)},
			{Name: "windows", OS: windows, Command: `Get-ItemProperty 'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*','HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*' -ErrorAction SilentlyContinue | Where-Object { $_.DisplayName -like ({{param}} + '*') } | Select-Object -First 1 -ExpandProperty DisplayVersion`, Parse: first()},
		},
	},

	// ---- web servers ----
	{
		ID: "nginx.version", Title: "nginx version", Category: "Web servers", Attr: "nginx.version", Type: model.TypeText,
		Variants: []Variant{{Name: "linux", OS: linux, Command: sbin("nginx -v 2>&1"), Parse: rx(`nginx/([0-9][0-9.]*)`)}},
	},
	{
		ID: "httpd.version", Title: "Apache httpd version", Category: "Web servers", Attr: "httpd.version", Type: model.TypeText,
		Variants: []Variant{{Name: "linux", OS: linux, Command: sbin("httpd -v 2>/dev/null || apache2 -v"), Parse: rx(`Apache/([0-9][0-9.]*)`)}},
	},

	// ---- templates that read a path or a variable ----
	{
		ID: "file.exists", Title: "File exists", Category: "Files & config", Type: model.TypeFlag,
		Description: "Records yes or no.",
		Param:       &Param{Kind: ParamPath, Label: "Path", Placeholder: "/etc/resolv.conf", Help: "A full path. On Windows, such as C:\\app\\config.xml."},
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "if [ -e {{param}} ]; then echo yes; else echo no; fi", Parse: first()},
			{Name: "windows", OS: windows, Command: "if (Test-Path -LiteralPath {{param}}) { 'yes' } else { 'no' }", Parse: first()},
		},
	},
	{
		ID: "file.contents", Title: "File contents", Category: "Files & config", Type: model.TypeText, Reveals: true,
		Description: "The whole file, up to 8 KB. Use it for small configuration files.",
		Param:       &Param{Kind: ParamPath, Label: "Path", Placeholder: "/etc/resolv.conf", Help: "A full path to a small text file."},
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: "cat {{param}}", Parse: everything()},
			{Name: "windows", OS: windows, Command: "Get-Content -LiteralPath {{param}} -Raw", Parse: everything()},
		},
	},
	{
		ID: "config.value", Title: "Setting in a config file", Category: "Files & config", Type: model.TypeText, Reveals: true,
		Description: "One key = value line from an .ini, .conf or .properties file, optionally only inside a [section]. Such as serverName in Splunk's server.conf.",
		Param: &Param{Kind: ParamConfig, Label: "File", Placeholder: "/opt/splunkforwarder/etc/system/local/server.conf",
			Help: "A full path to the file.", KeyPlaceholder: "serverName", SectionPlaceholder: "general"},
		Variants: []Variant{
			{Name: "unix", OS: unix, Command: configAwk, Parse: first()},
			{Name: "windows", OS: windows, Command: configPowerShell, Parse: first()},
		},
	},
	{
		ID: "env.var", Title: "Environment variable", Category: "Files & config", Type: model.TypeText, Reveals: true,
		Description: "A system-wide variable, such as SPLUNK_HOME or JAVA_HOME. On Linux and AIX it is read after /etc/profile; on Windows from the machine environment.",
		Param:       &Param{Kind: ParamEnvVar, Label: "Variable", Placeholder: "SPLUNK_HOME", Help: "Letters, digits and underscores."},
		Variants: []Variant{
			// The name is not quoted: it has been checked to be an identifier,
			// and quoting it would stop the shell expanding it.
			{Name: "unix", OS: unix, Command: `. /etc/profile >/dev/null 2>&1; echo "${{{name}}}"`, Parse: first()},
			{Name: "windows", OS: windows, Command: "[Environment]::GetEnvironmentVariable({{param}}, 'Machine')", Parse: first()},
		},
	},
}

// Lookup finds a catalog entry by id.
func Lookup(id string) (Template, bool) {
	for _, t := range Catalog {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

var (
	rePath    = regexp.MustCompile(`^(/|[A-Za-z]:\\)[A-Za-z0-9 ._+,@=~:()\\/-]*$`)
	reEnvVar  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	rePackage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._+-]{0,99}$`)
	reAttr    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_-]+)+$`)
	reSlugAny = regexp.MustCompile(`[^a-z0-9]+`)
	reConfKey = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	reSection = regexp.MustCompile(`^[A-Za-z0-9 _.:/=-]{0,100}$`)
)

// ConfigParam joins a file, section and key into the one value a
// "Setting in a config file" choice stores.
func ConfigParam(path, section, key string) string {
	path, section, key = strings.TrimSpace(path), strings.TrimSpace(section), strings.TrimSpace(key)
	section = strings.TrimSuffix(strings.TrimPrefix(section, "["), "]")
	if section != "" {
		return path + "#[" + section + "]" + key
	}
	return path + "#" + key
}

// SplitConfigParam takes apart a value ConfigParam made.
func SplitConfigParam(v string) (path, section, key string, err error) {
	path, rest, ok := strings.Cut(v, "#")
	if !ok || path == "" || rest == "" {
		return "", "", "", errors.New("name the file and the key, such as /etc/app.conf#port or /etc/app.conf#[server]port")
	}
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return "", "", "", errors.New("a [section] is missing its closing ]")
		}
		section, rest = rest[1:end], rest[end+1:]
	}
	if rest == "" {
		return "", "", "", errors.New("name the key to read")
	}
	return path, section, rest, nil
}

// ParamText shows a choice's value the way a person would say it, such as
// "server.conf [general] serverName".
func ParamText(t Template, v string) string {
	if t.Param == nil || !t.Param.Config() {
		return v
	}
	path, section, key, err := SplitConfigParam(v)
	if err != nil {
		return v
	}
	if section != "" {
		return path + " [" + section + "] " + key
	}
	return path + " " + key
}

// CheckParam validates a template's value. The patterns are narrow on
// purpose: they admit what real paths, variable and package names look like
// and nothing that means anything to a shell inside single quotes.
func CheckParam(kind ParamKind, v string) error {
	if v == "" {
		return errors.New("a value is required")
	}
	if strings.Contains(v, "..") {
		return errors.New("a path may not contain ..")
	}
	var ok bool
	var what string
	switch kind {
	case ParamPath:
		ok, what = len(v) <= 255 && rePath.MatchString(v),
			"a full path, starting with / or a drive letter, of letters, digits, spaces and . _ - + , @ = ~ : ( ) / \\"
	case ParamEnvVar:
		ok, what = reEnvVar.MatchString(v), "letters, digits and underscores, not starting with a digit"
	case ParamPackage:
		ok, what = rePackage.MatchString(v), "letters, digits, spaces and . _ + -"
	case ParamConfig:
		path, section, key, err := SplitConfigParam(v)
		if err != nil {
			return err
		}
		if err := CheckParam(ParamPath, path); err != nil {
			return err
		}
		if !reConfKey.MatchString(key) {
			return fmt.Errorf("key %q is not allowed: use letters, digits and . _ -", key)
		}
		if !reSection.MatchString(section) {
			return fmt.Errorf("section %q is not allowed: use letters, digits, spaces and . _ - : / =", section)
		}
		return nil
	default:
		return fmt.Errorf("unknown kind of value %q", kind)
	}
	if !ok {
		return fmt.Errorf("%q is not allowed: use %s", v, what)
	}
	return nil
}

// CheckAttr validates an attribute name a person chose.
func CheckAttr(a string) error {
	if !reAttr.MatchString(a) {
		return fmt.Errorf("%q is not a usable attribute name: use lowercase words joined by dots, such as app.version", a)
	}
	return nil
}

// DefaultAttr is the attribute a template records when the person does not
// name one, such as file.resolv_conf.exists for /etc/resolv.conf.
func DefaultAttr(t Template, param string) string {
	if t.Fixed() {
		return t.Attr
	}
	slugOf := func(s string) string {
		return strings.Trim(reSlugAny.ReplaceAllString(strings.ToLower(s), "_"), "_")
	}
	if t.Param.Config() {
		path, section, key, err := SplitConfigParam(param)
		if err != nil {
			return "conf.item"
		}
		attr := "conf." + slugOf(path[strings.LastIndexAny(path, `/\`)+1:])
		if s := slugOf(section); s != "" {
			attr += "." + s
		}
		if k := slugOf(key); k != "" {
			attr += "." + k
		}
		return attr
	}
	base := param
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.Trim(reSlugAny.ReplaceAllString(strings.ToLower(base), "_"), "_")
	if base == "" {
		base = "item"
	}
	switch t.ID {
	case "file.exists":
		return "file." + base + ".exists"
	case "file.contents":
		return "file." + base + ".contents"
	case "env.var":
		return "env." + base
	case "package.version":
		return "pkg." + base + ".version"
	}
	return t.ID + "." + base
}

// quote wraps a checked value in single quotes. Within them neither sh nor
// PowerShell expands anything, and CheckParam has refused the quote itself.
func quote(v string) string { return "'" + v + "'" }

// Choice is a catalog entry a person switched on.
type Choice struct {
	ID       int64
	Template string
	Param    string
	Attr     string
	Stacks   []string
}

// Specs turns a choice into the specs that run, one per variant.
func (c Choice) Specs() ([]Spec, error) {
	t, ok := Lookup(c.Template)
	if !ok {
		return nil, fmt.Errorf("no catalog entry %q", c.Template)
	}
	if !t.Fixed() {
		if err := CheckParam(t.Param.Kind, c.Param); err != nil {
			return nil, err
		}
	}
	attr := c.Attr
	if attr == "" {
		attr = DefaultAttr(t, c.Param)
	}
	if err := CheckAttr(attr); err != nil {
		return nil, err
	}
	out := make([]Spec, 0, len(t.Variants))
	for _, v := range t.Variants {
		cmd := v.Command
		if !t.Fixed() {
			if t.Param.Config() {
				path, section, key, _ := SplitConfigParam(c.Param)
				cmd = strings.ReplaceAll(cmd, "{{path}}", quote(path))
				cmd = strings.ReplaceAll(cmd, "{{section}}", quote(section))
				cmd = strings.ReplaceAll(cmd, "{{key}}", quote(key))
			}
			cmd = strings.ReplaceAll(cmd, "{{param}}", quote(c.Param))
			cmd = strings.ReplaceAll(cmd, "{{name}}", c.Param)
		}
		out = append(out, Spec{
			ID:       c.SpecID(v.Name),
			Attr:     attr,
			OS:       v.OS,
			Comment:  t.Title,
			Command:  cmd,
			Parse:    v.Parse,
			Type:     t.Type,
			Optional: true,
			Stacks:   c.Stacks,
		})
	}
	return out, nil
}

// SpecID names the spec for one variant. A fixed entry's id is readable and
// stable; a template's carries the choice number, because the same template
// may be switched on for several paths.
func (c Choice) SpecID(variant string) string {
	t, _ := Lookup(c.Template)
	if t.Fixed() {
		return "catalog." + c.Template + "." + variant
	}
	return "catalog." + c.Template + "." + strconv.FormatInt(c.ID, 10) + "." + variant
}

// Label describes a choice in a sentence, for lists.
func (c Choice) Label() string {
	t, ok := Lookup(c.Template)
	if !ok {
		return c.Template
	}
	if t.Fixed() {
		return t.Title
	}
	return t.Title + ": " + c.ParamText()
}

// ParamText is the choice's value as a person would say it.
func (c Choice) ParamText() string {
	t, ok := Lookup(c.Template)
	if !ok {
		return c.Param
	}
	return ParamText(t, c.Param)
}

// Group is a set of fixed catalog entries switched on together, such as
// everything worth knowing about a Splunk forwarder.
type Group struct {
	ID          string
	Title       string
	Description string
	Entries     []string
}

// Groups are offered above the catalog, in this order.
var Groups = []Group{
	{ID: "group.basics", Title: "Host basics", Description: "CPU cores, memory, time zone and DNS servers.",
		Entries: []string{"cpu.cores", "memory.total", "timezone", "dns.servers"}},
	{ID: "group.splunk.forwarder", Title: "Splunk forwarder", Description: "The Universal Forwarder's version and where it is installed.",
		Entries: []string{"splunk.uf.version", "splunk.home"}},
	{ID: "group.splunk.enterprise", Title: "Splunk Enterprise", Description: "The Splunk Enterprise version and where it is installed.",
		Entries: []string{"splunk.enterprise.version", "splunk.home"}},
	{ID: "group.runtimes", Title: "Language runtimes", Description: "Java, Python and OpenSSL versions.",
		Entries: []string{"java.version", "python.version", "openssl.version"}},
	{ID: "group.web", Title: "Web servers", Description: "nginx and Apache httpd versions.",
		Entries: []string{"nginx.version", "httpd.version"}},
}

// LookupGroup finds a group by id.
func LookupGroup(id string) (Group, bool) {
	for _, g := range Groups {
		if g.ID == id {
			return g, true
		}
	}
	return Group{}, false
}

// Titles lists the titles of a group's entries.
func (g Group) Titles() []string {
	out := make([]string, 0, len(g.Entries))
	for _, id := range g.Entries {
		if t, ok := Lookup(id); ok {
			out = append(out, t.Title)
		}
	}
	return out
}

// ShortLabel names what a choice records in a few words, for a column
// heading: "openssl package version" rather than pkg.openssl.version.
func (c Choice) ShortLabel() string {
	t, ok := Lookup(c.Template)
	if !ok {
		return c.Attr
	}
	if t.Fixed() {
		return t.Title
	}
	switch t.ID {
	case "file.exists":
		return c.Param + " exists"
	case "file.contents":
		return "Contents of " + c.Param
	case "env.var":
		return "Variable " + c.Param
	case "package.version":
		return c.Param + " package version"
	case "config.value":
		path, section, key, err := SplitConfigParam(c.Param)
		if err != nil {
			return c.Param
		}
		file := path[strings.LastIndexAny(path, `/\`)+1:]
		if section != "" {
			return key + " in " + file + " [" + section + "]"
		}
		return key + " in " + file
	}
	return c.Label()
}

// FixedTitles maps the attribute each fixed catalog entry records to the
// entry's title, so a page can head a column "Memory total (MB)" rather than
// spelling out hw.memory.total_mb.
func FixedTitles() map[string]string {
	out := map[string]string{}
	for _, t := range Catalog {
		if t.Fixed() && t.Attr != "" {
			out[t.Attr] = t.Title
		}
	}
	return out
}
