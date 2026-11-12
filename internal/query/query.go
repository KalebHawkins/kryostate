// Package query is the inventory's filter language.
//
// A query is a boolean expression over one host's current values, in the
// spirit of a PromQL label matcher:
//
//	os.family == "Linux" && !file.exists["/opt/splunkforwarder/bin/splunk"]
//
// The grammar, in EBNF:
//
//	query      = or ;
//	or         = and { "||" and } ;
//	and        = unary { "&&" unary } ;
//	unary      = "!" unary | primary ;
//	primary    = "(" or ")"
//	           | "has" "(" field ")"
//	           | field [ comparison literal ] ;
//	field      = name [ "[" string "]" ] ;
//	comparison = "==" | "!=" | "=~" | "!~" | "<" | "<=" | ">" | ">=" ;
//	literal    = string | number | "true" | "false" ;
//	name       = letter { letter | digit | "_" | "." | "-" } ;
//	string     = '"' chars '"' | "'" chars "'" | "`" raw chars "`" ;
//	number     = [ "-" ] digit { digit } [ "." digit { digit } ] ;
//
// "!" binds looser than a comparison, so !a == "x" means !(a == "x").
// && binds tighter than ||.
//
// Names. name, id, stack, role and application (or app) are the host's name,
// its id, the stacks it is a member of, its roles in them (and its role
// attribute, if one was recorded), and its app.name attribute. Any other name
// is an attribute, such as os.version. A parameterised collector, such as
// file.exists, is named with its parameter in brackets:
// file.exists["/opt/x/bin/x"] is whatever attribute that collector records
// for that path. As a shorthand, file.exists == "/opt/x/bin/x" means the same
// as file.exists["/opt/x/bin/x"] == true, and for collectors that do not
// record yes or no, package.version == "openssl" means the package has a
// recorded version.
//
// Semantics. A missing value compares as the empty string, so attr != "" and
// has(attr) both test presence. == against a string is exact and case
// sensitive. == against true or false reads true/yes and false/no. =~ and !~
// use Go regular expressions and are unanchored: "^web" matches names that
// start with web, "web" matches names containing it. <, <=, > and >= compare
// numerically when both sides are numbers, and in version order (7.10 > 7.9)
// when both look like versions; anything else does not match. Names with
// several values (stack, role) match when any value does, and != and !~ are
// exactly the negation of == and =~, so stack != "x" means "not in stack x".
// A bare name with no comparison means name == true, for yes/no attributes.
package query

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

// Host is what a query is evaluated against.
type Host struct {
	ID     string
	Name   string
	Stacks []string
	Roles  []string
	// Values are the current host-wide values, attribute to value.
	Values map[string]string
}

// Resolver names the attribute a parameterised collector records for one
// parameter. The store knows when a person chose a custom name; without one,
// collect.DefaultAttr is the answer.
type Resolver func(template, param string) string

// Expr is a compiled query.
type Expr struct {
	src  string
	root node
}

// String returns the query as written.
func (e *Expr) String() string { return e.src }

// Match reports whether a host satisfies the query.
func (e *Expr) Match(h Host) bool { return e.root.eval(&h) }

// Parse compiles a query, naming parameterised attributes the way
// collect.DefaultAttr does.
func Parse(src string) (*Expr, error) { return Compile(src, nil) }

// Compile compiles a query. A nil resolver uses collect.DefaultAttr.
func Compile(src string, resolve Resolver) (*Expr, error) {
	if strings.TrimSpace(src) == "" {
		return nil, errAt(src, 0, "the query is empty")
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	if resolve == nil {
		resolve = func(tmpl, param string) string {
			t, _ := collect.Lookup(tmpl)
			return collect.DefaultAttr(t, param)
		}
	}
	p := &parser{src: src, toks: toks, resolve: resolve}
	root, err := p.or()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		if t.kind == tRParen {
			return nil, errAt(src, t.off, ") has no matching (")
		}
		return nil, errAt(src, t.off, "unexpected %s after a complete condition; join conditions with && or ||", t.describe())
	}
	return &Expr{src: src, root: root}, nil
}

// LooksLikeQuery reports whether s uses any of the query syntax. Text that
// does not is treated by the inventory as a plain name search, which is what
// the search box did before there was a language.
func LooksLikeQuery(s string) bool {
	return strings.ContainsAny(s, "=!<>~&|()[]\"'`")
}

type parser struct {
	src     string
	toks    []token
	pos     int
	resolve Resolver
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) errf(t token, format string, args ...any) error {
	return errAt(p.src, t.off, format, args...)
}

func (p *parser) or() (node, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tOr {
		p.next()
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = orNode{l, r}
	}
	return l, nil
}

func (p *parser) and() (node, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tAnd {
		p.next()
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = andNode{l, r}
	}
	return l, nil
}

func (p *parser) unary() (node, error) {
	if p.peek().kind == tNot {
		p.next()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{x}, nil
	}
	return p.primary()
}

func (p *parser) primary() (node, error) {
	t := p.peek()
	switch t.kind {
	case tLParen:
		p.next()
		x, err := p.or()
		if err != nil {
			return nil, err
		}
		if c := p.peek(); c.kind != tRParen {
			return nil, p.errf(c, "expected ) to close the ( at column %d, found %s", errAt(p.src, t.off, "").Col, c.describe())
		}
		p.next()
		return x, nil
	case tIdent:
		if t.text == "has" && p.toks[p.pos+1].kind == tLParen {
			return p.has()
		}
		return p.comparison()
	case tEOF:
		if p.pos == 0 {
			return nil, p.errf(t, "the query is empty")
		}
		return nil, p.errf(t, "the query ends too soon: expected a condition, such as os.family == \"Linux\"")
	case tString, tNumber, tTrue, tFalse:
		return nil, p.errf(t, "a condition starts with a name, such as os.version or stack, not a %s; write name == %s", t.kind, quoteLit(t))
	}
	return nil, p.errf(t, "expected a condition, such as os.family == \"Linux\", found %s", t.describe())
}

func (p *parser) has() (node, error) {
	p.next() // has
	open := p.next()
	f, tmpl, err := p.field()
	if err != nil {
		return nil, err
	}
	if tmpl != nil {
		return nil, p.errf(tmpl.tok, "%s needs its parameter here: has(%s[\"...\"])", tmpl.tok.text, tmpl.tok.text)
	}
	if c := p.peek(); c.kind != tRParen {
		return nil, p.errf(c, "expected ) to close has( at column %d, found %s", errAt(p.src, open.off, "").Col, c.describe())
	}
	p.next()
	return hasNode{f}, nil
}

// bareTemplate is a parameterised collector named without its parameter.
// Only a comparison against a string can complete it.
type bareTemplate struct {
	tok token
	t   collect.Template
}

func (p *parser) field() (field, *bareTemplate, error) {
	t := p.peek()
	if t.kind != tIdent {
		return field{}, nil, p.errf(t, "expected a name, such as os.version, found %s", t.describe())
	}
	p.next()
	tmpl, isTemplate := collect.Lookup(t.text)
	if isTemplate && tmpl.Fixed() {
		isTemplate = false
	}
	if p.peek().kind == tLBrack {
		open := p.next()
		if !isTemplate {
			return field{}, nil, p.errf(open, "only a collector that takes a parameter can be indexed with [...]; %s is not one (those are %s)", t.text, paramTemplates())
		}
		arg := p.next()
		if arg.kind != tString {
			return field{}, nil, p.errf(arg, "expected the %s in quotes inside [...], found %s", paramWord(tmpl), arg.describe())
		}
		if c := p.peek(); c.kind != tRBrack {
			return field{}, nil, p.errf(c, "expected ] to close the [ at column %d, found %s", errAt(p.src, open.off, "").Col, c.describe())
		}
		p.next()
		return field{kind: fAttr, attr: p.resolve(tmpl.ID, arg.text)}, nil, nil
	}
	if isTemplate {
		return field{}, &bareTemplate{tok: t, t: tmpl}, nil
	}
	switch t.text {
	case "name":
		return field{kind: fName}, nil, nil
	case "id":
		return field{kind: fID}, nil, nil
	case "stack":
		return field{kind: fStack}, nil, nil
	case "role":
		return field{kind: fRole}, nil, nil
	case "application", "app":
		return field{kind: fAttr, attr: "app.name"}, nil, nil
	}
	return field{kind: fAttr, attr: t.text}, nil, nil
}

func (p *parser) comparison() (node, error) {
	f, tmpl, err := p.field()
	if err != nil {
		return nil, err
	}
	op := p.peek()
	if !op.kind.comparison() {
		if tmpl != nil {
			return nil, p.errf(tmpl.tok, "%s takes a %s: write %s[\"...\"] == true, or %s == \"...\"", tmpl.tok.text, paramWord(tmpl.t), tmpl.tok.text, tmpl.tok.text)
		}
		if op.kind == tIdent || op.kind == tString || op.kind == tNumber {
			return nil, p.errf(op, "expected a comparison such as == after the name, found %s", op.describe())
		}
		// A bare name is a yes/no test.
		return cmpNode{f: f, op: tEq, lit: literal{isBool: true, b: true}}, nil
	}
	p.next()
	lt := p.next()
	var lit literal
	switch lt.kind {
	case tString:
		lit = literal{s: lt.text}
	case tNumber:
		n, err := strconv.ParseFloat(lt.text, 64)
		if err != nil {
			return nil, p.errf(lt, "%s is not a number", lt.text)
		}
		lit = literal{s: lt.text, isNum: true, n: n}
	case tTrue, tFalse:
		lit = literal{s: lt.text, isBool: true, b: lt.kind == tTrue}
	case tEOF:
		return nil, p.errf(lt, "expected a value after %s, such as \"Linux\", 8 or true; the query ends here", op.kind)
	case tIdent:
		return nil, p.errf(lt, "expected a value after %s, found %s; put text in double quotes: \"%s\"", op.kind, lt.describe(), lt.text)
	default:
		return nil, p.errf(lt, "expected a value after %s, such as \"Linux\", 8 or true, found %s", op.kind, lt.describe())
	}

	if tmpl != nil {
		if (op.kind != tEq && op.kind != tNe) || lt.kind != tString {
			return nil, p.errf(op, "%s takes a %s: write %s[\"...\"] %s ..., or %s == \"...\" to test it", tmpl.tok.text, paramWord(tmpl.t), tmpl.tok.text, op.kind, tmpl.tok.text)
		}
		attr := field{kind: fAttr, attr: p.resolve(tmpl.t.ID, lt.text)}
		var n node = hasNode{attr}
		if tmpl.t.Type == "flag" {
			n = cmpNode{f: attr, op: tEq, lit: literal{isBool: true, b: true}}
		}
		if op.kind == tNe {
			n = notNode{n}
		}
		return n, nil
	}

	c := cmpNode{f: f, op: op.kind, lit: lit}
	switch op.kind {
	case tMatch, tNotMatch:
		if lt.kind != tString {
			return nil, p.errf(lt, "%s takes a pattern in quotes, such as \"^web\"", op.kind)
		}
		re, err := regexp.Compile(lt.text)
		if err != nil {
			msg := err.Error()
			msg = strings.TrimPrefix(msg, "error parsing regexp: ")
			return nil, p.errf(lt, "not a valid pattern: %s", msg)
		}
		c.re = re
	case tLt, tLe, tGt, tGe:
		if lit.isBool {
			return nil, p.errf(lt, "%s compares numbers and versions, not true or false", op.kind)
		}
		if !lit.isNum && !isNumber(lit.s) && !looksVersion(lit.s) {
			return nil, p.errf(lt, "%s compares numbers and versions; %q is neither", op.kind, lit.s)
		}
	}
	return c, nil
}

func quoteLit(t token) string {
	if t.kind == tString {
		return strconv.Quote(t.text)
	}
	return t.text
}

func paramWord(t collect.Template) string {
	if t.Param != nil && t.Param.Kind != "" {
		switch t.Param.Kind {
		case collect.ParamEnvVar:
			return "variable name"
		case collect.ParamPackage:
			return "package name"
		case collect.ParamConfig:
			return "file#key parameter"
		}
		return string(t.Param.Kind)
	}
	return "parameter"
}

func paramTemplates() string {
	var names []string
	for _, t := range collect.Catalog {
		if !t.Fixed() {
			names = append(names, t.ID)
		}
	}
	return strings.Join(names, ", ")
}

// ---- evaluation ----

type node interface{ eval(h *Host) bool }

type andNode struct{ l, r node }
type orNode struct{ l, r node }
type notNode struct{ x node }

func (n andNode) eval(h *Host) bool { return n.l.eval(h) && n.r.eval(h) }
func (n orNode) eval(h *Host) bool  { return n.l.eval(h) || n.r.eval(h) }
func (n notNode) eval(h *Host) bool { return !n.x.eval(h) }

type fieldKind int

const (
	fAttr fieldKind = iota
	fName
	fID
	fStack
	fRole
)

type field struct {
	kind fieldKind
	attr string
}

func (f field) values(h *Host) []string {
	switch f.kind {
	case fName:
		return []string{h.Name}
	case fID:
		return []string{h.ID}
	case fStack:
		return h.Stacks
	case fRole:
		out := h.Roles
		if v := h.Values["role"]; v != "" {
			out = append(append([]string(nil), out...), v)
		}
		return out
	}
	if v, ok := h.Values[f.attr]; ok && v != "" {
		return []string{v}
	}
	return nil
}

type hasNode struct{ f field }

func (n hasNode) eval(h *Host) bool {
	for _, v := range n.f.values(h) {
		if v != "" {
			return true
		}
	}
	return false
}

type literal struct {
	s      string
	isNum  bool
	n      float64
	isBool bool
	b      bool
}

type cmpNode struct {
	f   field
	op  kind
	lit literal
	re  *regexp.Regexp
}

func (n cmpNode) eval(h *Host) bool {
	vals := n.f.values(h)
	switch n.op {
	case tNe:
		return !n.any(vals, n.equal)
	case tNotMatch:
		return !n.any(vals, n.re.MatchString)
	case tEq:
		return n.any(vals, n.equal)
	case tMatch:
		return n.any(vals, n.re.MatchString)
	}
	return n.anyPresent(vals, func(v string) bool {
		c, ok := order(v, n.lit)
		if !ok {
			return false
		}
		switch n.op {
		case tLt:
			return c < 0
		case tLe:
			return c <= 0
		case tGt:
			return c > 0
		case tGe:
			return c >= 0
		}
		return false
	})
}

// any applies a test to every value, testing the empty string for a missing
// one, so that attr == "" finds the hosts without it.
func (n cmpNode) any(vals []string, test func(string) bool) bool {
	if len(vals) == 0 {
		return test("")
	}
	for _, v := range vals {
		if test(v) {
			return true
		}
	}
	return false
}

func (n cmpNode) anyPresent(vals []string, test func(string) bool) bool {
	for _, v := range vals {
		if test(v) {
			return true
		}
	}
	return false
}

func (n cmpNode) equal(v string) bool {
	switch {
	case n.lit.isBool:
		b, ok := parseBool(v)
		return ok && b == n.lit.b
	case n.lit.isNum:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && f == n.lit.n
	}
	if v == n.lit.s {
		return true
	}
	// "yes" and "true" are the same answer while collectors move from one
	// spelling to the other.
	if a, ok := parseBool(v); ok {
		if b, ok := parseBool(n.lit.s); ok {
			return a == b
		}
	}
	return false
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes":
		return true, true
	case "false", "no":
		return false, true
	}
	return false, false
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && strings.TrimSpace(s) != ""
}

// order compares a value against a literal, numerically when both are
// numbers and in version order when both look like versions.
func order(v string, lit literal) (int, bool) {
	v = strings.TrimSpace(v)
	b, berr := lit.n, error(nil)
	if !lit.isNum {
		b, berr = strconv.ParseFloat(strings.TrimSpace(lit.s), 64)
	}
	if a, err := strconv.ParseFloat(v, 64); err == nil && berr == nil {
		switch {
		case a < b:
			return -1, true
		case a > b:
			return 1, true
		}
		return 0, true
	}
	if looksVersion(v) && looksVersion(lit.s) {
		return CompareVersions(v, lit.s), true
	}
	return 0, false
}

var reVersion = regexp.MustCompile(`^[vV]?[0-9][0-9A-Za-z.+_~:-]*$`)

func looksVersion(s string) bool { return reVersion.MatchString(strings.TrimSpace(s)) }

// CompareVersions orders two version strings: runs of digits compare as
// numbers, runs of letters as text, and punctuation only separates. A number
// sorts after letters at the same place, and a version that
// is a prefix of another sorts first, so 7.9 < 7.9.1 and 1.0.2 < 1.0.2k
// (a pre-release suffix such as rc1 therefore sorts after the release).
func CompareVersions(a, b string) int {
	ta, tb := versionParts(a), versionParts(b)
	for i := 0; i < len(ta) && i < len(tb); i++ {
		x, y := ta[i], tb[i]
		dx, dy := isDigit(x[0]), isDigit(y[0])
		switch {
		case dx && dy:
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(x) != len(y) {
				if len(x) < len(y) {
					return -1
				}
				return 1
			}
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		case dx:
			return 1
		case dy:
			return -1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(ta) < len(tb):
		return -1
	case len(ta) > len(tb):
		return 1
	}
	return 0
}

func versionParts(s string) []string {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "v"), "V")
	var out []string
	i := 0
	for i < len(s) {
		c := s[i]
		j := i
		switch {
		case isDigit(c):
			for j < len(s) && isDigit(s[j]) {
				j++
			}
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
				j++
			}
		default:
			i++
			continue
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

// Quote returns s as a string literal for a query, for building links.
func Quote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case '\n':
			sb.WriteString(`\n`)
		case '\t':
			sb.WriteString(`\t`)
		case '\r':
			sb.WriteString(`\r`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}
