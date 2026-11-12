package query

import "sort"

// Attrs lists the attributes a query reads, by the names their values are
// recorded under, sorted and each once. A parameterised collector appears as
// the attribute it records for its parameter, so
// file.exists["/etc/hosts"] == true lists that attribute. name, id, stack and
// role are not attributes and are never listed.
func (e *Expr) Attrs() []string {
	seen := map[string]bool{}
	walk(e.root, func(f field) {
		if f.kind == fAttr && f.attr != "" {
			seen[f.attr] = true
		}
	})
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Missing lists the attributes the query reads that the host has no value
// for, in the order Attrs gives them.
func (e *Expr) Missing(h Host) []string {
	var out []string
	for _, a := range e.Attrs() {
		if h.Values[a] == "" {
			out = append(out, a)
		}
	}
	return out
}

// Verdict is what a query says about one host when a missing value is not
// taken to be an answer.
type Verdict int

const (
	// No: the host's values settle it, and it does not match.
	No Verdict = iota
	// Yes: the host matches.
	Yes
	// Unknown: the answer hangs on a value the host has not reported.
	Unknown
)

func (v Verdict) String() string {
	switch v {
	case Yes:
		return "yes"
	case Unknown:
		return "unknown"
	}
	return "no"
}

// Decide evaluates the query in three-valued logic. Match reads a missing
// value as the empty string, which is right for a filter; a rollout needs to
// tell "this host is not done" from "this host has not said". Here a
// comparison against a value the host has no record of is Unknown, unless it
// asks about absence itself: has(x), x == "" and x != "" always have an
// answer. && is No if either side is, || is Yes if either side is, and ! keeps
// Unknown as it is.
func (e *Expr) Decide(h Host) Verdict { return decide(e.root, &h) }

func decide(n node, h *Host) Verdict {
	switch n := n.(type) {
	case andNode:
		l := decide(n.l, h)
		if l == No {
			return No
		}
		r := decide(n.r, h)
		switch {
		case r == No:
			return No
		case l == Yes && r == Yes:
			return Yes
		}
		return Unknown
	case orNode:
		l := decide(n.l, h)
		if l == Yes {
			return Yes
		}
		r := decide(n.r, h)
		switch {
		case r == Yes:
			return Yes
		case l == No && r == No:
			return No
		}
		return Unknown
	case notNode:
		switch decide(n.x, h) {
		case Yes:
			return No
		case No:
			return Yes
		}
		return Unknown
	case inNode:
		out := No
		for _, t := range n.tests {
			switch decide(t, h) {
			case Yes:
				return Yes
			case Unknown:
				out = Unknown
			}
		}
		return out
	case cmpNode:
		if n.f.kind == fAttr && len(n.f.values(h)) == 0 {
			// Comparing against the empty string asks whether there is a
			// value, and that has an answer.
			if (n.op == tEq || n.op == tNe) && !n.lit.isNum && !n.lit.isBool && n.lit.s == "" {
				break
			}
			return Unknown
		}
	}
	if n.eval(h) {
		return Yes
	}
	return No
}

// walk visits every field a query reads.
func walk(n node, visit func(field)) {
	switch n := n.(type) {
	case andNode:
		walk(n.l, visit)
		walk(n.r, visit)
	case orNode:
		walk(n.l, visit)
		walk(n.r, visit)
	case notNode:
		walk(n.x, visit)
	case inNode:
		for _, t := range n.tests {
			walk(t, visit)
		}
	case hasNode:
		visit(n.f)
	case cmpNode:
		visit(n.f)
	}
}
