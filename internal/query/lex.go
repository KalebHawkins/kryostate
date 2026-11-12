package query

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type kind int

const (
	tEOF kind = iota
	tIdent
	tString
	tNumber
	tTrue
	tFalse
	tEq       // ==
	tNe       // !=
	tMatch    // =~
	tNotMatch // !~
	tLt       // <
	tLe       // <=
	tGt       // >
	tGe       // >=
	tAnd      // &&
	tOr       // ||
	tNot      // !
	tLParen
	tRParen
	tLBrack
	tRBrack
	tComma
)

func (k kind) String() string {
	switch k {
	case tEOF:
		return "end of query"
	case tIdent:
		return "name"
	case tString:
		return "string"
	case tNumber:
		return "number"
	case tTrue:
		return "true"
	case tFalse:
		return "false"
	case tEq:
		return "=="
	case tNe:
		return "!="
	case tMatch:
		return "=~"
	case tNotMatch:
		return "!~"
	case tLt:
		return "<"
	case tLe:
		return "<="
	case tGt:
		return ">"
	case tGe:
		return ">="
	case tAnd:
		return "&&"
	case tOr:
		return "||"
	case tNot:
		return "!"
	case tLParen:
		return "("
	case tRParen:
		return ")"
	case tLBrack:
		return "["
	case tRBrack:
		return "]"
	case tComma:
		return ","
	}
	return "?"
}

func (k kind) comparison() bool {
	switch k {
	case tEq, tNe, tMatch, tNotMatch, tLt, tLe, tGt, tGe:
		return true
	}
	return false
}

type token struct {
	kind kind
	text string // identifier name, decoded string, or number text
	off  int    // byte offset of the first character
}

// describe names a token for an error message.
func (t token) describe() string {
	switch t.kind {
	case tEOF:
		return "end of query"
	case tIdent:
		return fmt.Sprintf("name %s", t.text)
	case tString:
		return fmt.Sprintf("string %q", t.text)
	case tNumber:
		return fmt.Sprintf("number %s", t.text)
	}
	return t.kind.String()
}

// Error is a problem in a query, with where it was found.
type Error struct {
	// Offset is the byte offset into the query; Col is the same position
	// as a 1-based count of characters, which is what a person counts.
	Offset int
	Col    int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("column %d: %s", e.Col, e.Msg) }

// Caret returns a line with a ^ under the problem, to print beneath the
// query.
func (e *Error) Caret() string { return strings.Repeat(" ", e.Col-1) + "^" }

func errAt(src string, off int, format string, args ...any) *Error {
	if off > len(src) {
		off = len(src)
	}
	return &Error{Offset: off, Col: utf8.RuneCountInString(src[:off]) + 1, Msg: fmt.Sprintf(format, args...)}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || isDigit(c) || c == '.' || c == '-'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// lex splits a query into tokens. It is the whole lexer: the grammar is small
// enough that a scanner with a state machine would be ceremony.
func lex(src string) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			word := src[i:j]
			k := tIdent
			switch word {
			case "true":
				k = tTrue
			case "false":
				k = tFalse
			}
			out = append(out, token{kind: k, text: word, off: i})
			i = j
			continue
		case isDigit(c) || (c == '-' && i+1 < len(src) && isDigit(src[i+1])):
			j := i + 1
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			if j < len(src) && src[j] == '.' {
				j++
				if j >= len(src) || !isDigit(src[j]) {
					return nil, errAt(src, i, "%s is not a number; quote it if it is a version, such as \"%s\"", src[i:j], src[i:j])
				}
				for j < len(src) && isDigit(src[j]) {
					j++
				}
			}
			if j < len(src) && (isIdentPart(src[j])) {
				end := j
				for end < len(src) && isIdentPart(src[end]) {
					end++
				}
				return nil, errAt(src, i, "%s is not a number; quote it, such as \"%s\"", src[i:end], src[i:end])
			}
			out = append(out, token{kind: tNumber, text: src[i:j], off: i})
			i = j
			continue
		case c == '"' || c == '\'':
			s, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tString, text: s, off: i})
			i += n
			continue
		case c == '`':
			end := strings.IndexByte(src[i+1:], '`')
			if end < 0 {
				return nil, errAt(src, i, "string is not closed: add a ` at the end")
			}
			out = append(out, token{kind: tString, text: src[i+1 : i+1+end], off: i})
			i += end + 2
			continue
		}

		two := ""
		if i+1 < len(src) {
			two = src[i : i+2]
		}
		switch two {
		case "==":
			out = append(out, token{kind: tEq, off: i})
		case "!=":
			out = append(out, token{kind: tNe, off: i})
		case "=~":
			out = append(out, token{kind: tMatch, off: i})
		case "!~":
			out = append(out, token{kind: tNotMatch, off: i})
		case "<=":
			out = append(out, token{kind: tLe, off: i})
		case ">=":
			out = append(out, token{kind: tGe, off: i})
		case "&&":
			out = append(out, token{kind: tAnd, off: i})
		case "||":
			out = append(out, token{kind: tOr, off: i})
		default:
			two = ""
		}
		if two != "" {
			i += 2
			continue
		}

		var k kind
		switch c {
		case '<':
			k = tLt
		case '>':
			k = tGt
		case '!':
			k = tNot
		case '(':
			k = tLParen
		case ')':
			k = tRParen
		case '[':
			k = tLBrack
		case ']':
			k = tRBrack
		case ',':
			k = tComma
		case '=':
			return nil, errAt(src, i, "a single = is not an operator: use == to compare, or =~ to match a pattern")
		case '&':
			return nil, errAt(src, i, "a single & is not an operator: use && for and")
		case '|':
			return nil, errAt(src, i, "a single | is not an operator: use || for or")
		default:
			r, _ := utf8.DecodeRuneInString(src[i:])
			return nil, errAt(src, i, "unexpected character %q; values that are not numbers go in double quotes", r)
		}
		out = append(out, token{kind: k, off: i})
		i++
	}
	out = append(out, token{kind: tEOF, off: len(src)})
	return out, nil
}

// lexString reads a quoted string starting at src[start], returning the
// decoded text and how many bytes it took.
func lexString(src string, start int) (string, int, error) {
	q := src[start]
	var sb strings.Builder
	i := start + 1
	for i < len(src) {
		c := src[i]
		switch {
		case c == q:
			return sb.String(), i + 1 - start, nil
		case c == '\\':
			if i+1 >= len(src) {
				return "", 0, errAt(src, i, "string is not closed: add a %c at the end", q)
			}
			switch e := src[i+1]; e {
			case '\\', '"', '\'', '`':
				sb.WriteByte(e)
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			default:
				return "", 0, errAt(src, i, "unknown escape \\%c: write \\\\%c for a backslash, or use a `raw string` for a pattern", e, e)
			}
			i += 2
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return "", 0, errAt(src, start, "string is not closed: add a %c at the end", q)
}
