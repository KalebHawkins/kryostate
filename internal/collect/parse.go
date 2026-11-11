// Package collect runs commands against hosts and turns their output into
// assertions.
//
// The division of labour here is deliberate. A transport moves bytes and
// holds credentials. A parser receives bounded output and holds nothing: it
// never sees a credential and never opens a connection. Keeping them apart
// means a parsing rule contributed by an operator cannot reach the network,
// and a transport failure cannot be mistaken for a parsing failure.
package collect

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ParseKind names a way of pulling one value out of command output. The set is
// small on purpose: an operator adding a collector should be choosing from a
// handful of shapes, not writing a program.
type ParseKind string

const (
	// ParseFirstLine takes the first non-empty line, trimmed.
	ParseFirstLine ParseKind = "first_line"
	// ParseRegex takes the named group "v", or the first capture group.
	ParseRegex ParseKind = "regex"
	// ParseKV reads shell-style KEY=value, stripping surrounding quotes.
	ParseKV ParseKind = "kv"
	// ParseColonKV reads "Key : value" lines, as printed by prtconf and by
	// PowerShell's default formatting.
	ParseColonKV ParseKind = "colon_kv"
	// ParseField takes whitespace-separated field N of the first line
	// containing a marker.
	ParseField ParseKind = "field"
	// ParseJSONPath walks a dotted path through a JSON document.
	ParseJSONPath ParseKind = "json_path"
)

// Parse describes how to extract a value. Fields not relevant to the chosen
// kind are ignored.
type Parse struct {
	Kind    ParseKind `json:"kind"`
	Key     string    `json:"key,omitempty"`
	Pattern string    `json:"pattern,omitempty"`
	Marker  string    `json:"marker,omitempty"`
	Field   int       `json:"field,omitempty"`
	Path    string    `json:"path,omitempty"`
}

// ErrNoMatch means the command answered but the rule found nothing. It is
// distinct from a transport error because the remedy is different: one is a
// broken rule, the other is an unreachable host.
var ErrNoMatch = errors.New("output did not contain the requested value")

var (
	reCacheMu sync.Mutex
	reCache   = map[string]*regexp.Regexp{}
)

func compile(pattern string) (*regexp.Regexp, error) {
	reCacheMu.Lock()
	defer reCacheMu.Unlock()
	if re, ok := reCache[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	reCache[pattern] = re
	return re, nil
}

// Extract pulls a single value out of command output.
func Extract(p Parse, out string) (string, error) {
	switch p.Kind {
	case ParseFirstLine, "":
		return firstLine(out)
	case ParseRegex:
		return byRegex(p.Pattern, out)
	case ParseKV:
		return byKV(p.Key, out)
	case ParseColonKV:
		return byColonKV(p.Key, out)
	case ParseField:
		return byField(p.Marker, p.Field, out)
	case ParseJSONPath:
		return byJSONPath(p.Path, out)
	default:
		return "", fmt.Errorf("unknown parse kind %q", p.Kind)
	}
}

func firstLine(out string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if v := strings.TrimSpace(sc.Text()); v != "" {
			return v, nil
		}
	}
	return "", ErrNoMatch
}

func byRegex(pattern, out string) (string, error) {
	if pattern == "" {
		return "", errors.New("regex parse needs a pattern")
	}
	re, err := compile(pattern)
	if err != nil {
		return "", err
	}
	m := re.FindStringSubmatch(out)
	if m == nil {
		return "", ErrNoMatch
	}
	for i, name := range re.SubexpNames() {
		if name == "v" && i < len(m) {
			return strings.TrimSpace(m[i]), nil
		}
	}
	if len(m) > 1 {
		return strings.TrimSpace(m[1]), nil
	}
	return strings.TrimSpace(m[0]), nil
}

func byKV(key, out string) (string, error) {
	if key == "" {
		return "", errors.New("kv parse needs a key")
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return unquote(strings.TrimSpace(v)), nil
	}
	return "", ErrNoMatch
}

func byColonKV(key, out string) (string, error) {
	if key == "" {
		return "", errors.New("colon_kv parse needs a key")
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		return strings.TrimSpace(v), nil
	}
	return "", ErrNoMatch
}

// byField finds the first line containing marker and returns field n,
// one-based, counting whitespace-separated columns.
func byField(marker string, n int, out string) (string, error) {
	if marker == "" {
		return "", errors.New("field parse needs a marker")
	}
	if n < 1 {
		return "", errors.New("field numbers are one-based")
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, marker) {
			continue
		}
		f := strings.Fields(line)
		if len(f) < n {
			continue
		}
		return f[n-1], nil
	}
	return "", ErrNoMatch
}

func byJSONPath(path, out string) (string, error) {
	if path == "" {
		return "", errors.New("json_path parse needs a path")
	}
	var doc any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return "", fmt.Errorf("parse json: %w", err)
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", ErrNoMatch
		}
		cur, ok = m[seg]
		if !ok {
			return "", ErrNoMatch
		}
	}
	switch v := cur.(type) {
	case string:
		return v, nil
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", v), "0"), "."), nil
	case bool:
		return fmt.Sprint(v), nil
	case nil:
		return "", ErrNoMatch
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
