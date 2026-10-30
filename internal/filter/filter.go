// Package filter parses a simple filter DSL into a parameterized SQL WHERE clause.
//
// Grammar (informal):
//
//	expr     = term (("AND" | "OR") term)*
//	term     = "(" expr ")"
//	         | field op value
//	         | field "IN" "(" value ("," value)* ")"
//	         | field "CONTAINS" value          (case-insensitive substring)
//	         | field "MATCHES"  value          (regexp, case-sensitive)
//	         | field "MATCHES*" value          (regexp, case-insensitive)
//	         | field "NOT" "MATCHES"  value
//	         | field "NOT" "MATCHES*" value
//	         | field ">" duration              (relative time: 10m, 1h, 2d)
//
//	field    = identifier | "data." identifier
//	op       = "=" | "!=" | "<" | ">" | "<=" | ">="
//	value    = quotedString | bareWord
package filter

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Result holds a parameterized SQL fragment and its bound arguments.
// Args use positional ? placeholders (SQLite / database/sql style).
type Result struct {
	SQL  string
	Args []any
}

// ExtractSources splits expr into an explicit source list (from a top-level
// "source = X" or "source IN (...)" clause joined by AND) and the remaining
// non-source expression. If no source clause is found, sources is nil and
// rest equals expr.
func ExtractSources(expr string) (sources []string, rest string) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, ""
	}
	clauses := splitTopLevelAnd(tokenise(expr))
	var restParts []string
	for _, clause := range clauses {
		if srcs, ok := asSourceClause(clause); ok {
			sources = append(sources, srcs...)
		} else {
			restParts = append(restParts, renderTokens(clause))
		}
	}
	return sources, strings.Join(restParts, " AND ")
}

// BuildExpr assembles a filter expression from a source list and a base expression.
// Nil/empty sources means no source constraint is added.
func BuildExpr(sources []string, base string) string {
	var parts []string
	if base != "" {
		parts = append(parts, base)
	}
	switch len(sources) {
	case 1:
		parts = append(parts, "source = "+sources[0])
	case 0:
	default:
		parts = append(parts, "source IN ("+strings.Join(sources, ", ")+")")
	}
	return strings.Join(parts, " AND ")
}

// splitTopLevelAnd splits tokens on AND at parenthesis depth 0.
func splitTopLevelAnd(tokens []token) [][]token {
	var result [][]token
	var cur []token
	depth := 0
	for _, t := range tokens {
		if t.kind == tokEOF {
			break
		}
		switch {
		case t.kind == tokLParen:
			depth++
			cur = append(cur, t)
		case t.kind == tokRParen:
			depth--
			cur = append(cur, t)
		case depth == 0 && t.kind == tokWord && strings.ToUpper(t.val) == "AND":
			if len(cur) > 0 {
				result = append(result, cur)
				cur = nil
			}
		default:
			cur = append(cur, t)
		}
	}
	if len(cur) > 0 {
		result = append(result, cur)
	}
	return result
}

// asSourceClause returns the source values if the token slice is exactly
// "source = X" or "source IN ( ... )".
func asSourceClause(tokens []token) ([]string, bool) {
	if len(tokens) < 3 {
		return nil, false
	}
	if tokens[0].kind != tokWord || strings.ToLower(tokens[0].val) != "source" {
		return nil, false
	}
	if tokens[1].kind == tokOp && tokens[1].val == "=" && len(tokens) == 3 {
		return []string{tokens[2].val}, true
	}
	if tokens[1].kind == tokWord && strings.ToUpper(tokens[1].val) == "IN" {
		if len(tokens) < 4 || tokens[2].kind != tokLParen {
			return nil, false
		}
		var srcs []string
		for i := 3; i < len(tokens) && tokens[i].kind != tokRParen; i++ {
			if tokens[i].kind != tokComma {
				srcs = append(srcs, tokens[i].val)
			}
		}
		return srcs, true
	}
	return nil, false
}

// renderTokens reconstructs a whitespace-separated string from tokens.
func renderTokens(tokens []token) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		if t.kind == tokString {
			parts[i] = `"` + t.val + `"`
		} else {
			parts[i] = t.val
		}
	}
	return strings.Join(parts, " ")
}

// Parse compiles the filter expression into SQL.
// startArg is accepted for API compatibility but ignored (? placeholders are positional).
func Parse(expr string, startArg int) (*Result, error) {
	p := &parser{tokens: tokenise(expr)}
	sql, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.tokens) && p.tokens[p.pos].kind != tokEOF {
		return nil, fmt.Errorf("unexpected token %q", p.tokens[p.pos].val)
	}
	return &Result{SQL: sql, Args: p.args}, nil
}

// --- Tokeniser ---

type tokKind int

const (
	tokWord tokKind = iota
	tokString
	tokLParen
	tokRParen
	tokComma
	tokOp
	tokEOF
)

type token struct {
	kind tokKind
	val  string
}

func tokenise(s string) []token {
	var tokens []token
	i := 0
	for i < len(s) {
		c := rune(s[i])
		if unicode.IsSpace(c) {
			i++
			continue
		}
		switch c {
		case '(':
			tokens = append(tokens, token{tokLParen, "("})
			i++
		case ')':
			tokens = append(tokens, token{tokRParen, ")"})
			i++
		case ',':
			tokens = append(tokens, token{tokComma, ","})
			i++
		case '"', '\'':
			q := c
			i++
			start := i
			for i < len(s) && rune(s[i]) != q {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			tokens = append(tokens, token{tokString, s[start:i]})
			if i < len(s) {
				i++ // closing quote
			}
		default:
			if c == '!' || c == '<' || c == '>' || c == '=' {
				j := i + 1
				if j < len(s) && (s[j] == '=' || s[j] == '>') {
					tokens = append(tokens, token{tokOp, s[i : j+1]})
					i = j + 1
				} else {
					tokens = append(tokens, token{tokOp, string(c)})
					i++
				}
				continue
			}
			start := i
			for i < len(s) && !unicode.IsSpace(rune(s[i])) &&
				s[i] != '(' && s[i] != ')' && s[i] != ',' &&
				s[i] != '=' && s[i] != '!' && s[i] != '<' && s[i] != '>' {
				i++
			}
			tokens = append(tokens, token{tokWord, s[start:i]})
		}
	}
	tokens = append(tokens, token{tokEOF, ""})
	return tokens
}

// --- Parser ---

type parser struct {
	tokens []token
	pos    int
	args   []any
}

func (p *parser) peek() token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return token{tokEOF, ""}
}

func (p *parser) consume() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) arg(v any) string {
	p.args = append(p.args, v)
	return "?"
}

func (p *parser) parseExpr() (string, error) {
	left, err := p.parseTerm()
	if err != nil {
		return "", err
	}
	for {
		t := p.peek()
		if t.kind != tokWord {
			break
		}
		upper := strings.ToUpper(t.val)
		if upper != "AND" && upper != "OR" {
			break
		}
		p.consume()
		right, err := p.parseTerm()
		if err != nil {
			return "", err
		}
		left = fmt.Sprintf("(%s %s %s)", left, upper, right)
	}
	return left, nil
}

func (p *parser) parseTerm() (string, error) {
	// NOT prefix — negates any following term: NOT (expr), NOT field op val, etc.
	if t := p.peek(); t.kind == tokWord && strings.ToUpper(t.val) == "NOT" {
		p.consume()
		inner, err := p.parseTerm()
		if err != nil {
			return "", err
		}
		return "NOT (" + inner + ")", nil
	}

	if p.peek().kind == tokLParen {
		p.consume()
		inner, err := p.parseExpr()
		if err != nil {
			return "", err
		}
		if p.peek().kind != tokRParen {
			return "", fmt.Errorf("expected ')'")
		}
		p.consume()
		return "(" + inner + ")", nil
	}

	field, err := p.parseField()
	if err != nil {
		return "", err
	}

	t := p.peek()
	if t.kind != tokWord && t.kind != tokOp {
		return "", fmt.Errorf("expected operator after %q", field)
	}

	upper := strings.ToUpper(t.val)

	// IN operator
	if upper == "IN" {
		p.consume()
		if p.peek().kind != tokLParen {
			return "", fmt.Errorf("expected '(' after IN")
		}
		p.consume()
		var placeholders []string
		for {
			v, err := p.parseValue()
			if err != nil {
				return "", err
			}
			placeholders = append(placeholders, p.arg(v))
			if p.peek().kind == tokComma {
				p.consume()
				continue
			}
			break
		}
		if p.peek().kind != tokRParen {
			return "", fmt.Errorf("expected ')' after IN list")
		}
		p.consume()
		return fmt.Sprintf("%s IN (%s)", toSQL(field), strings.Join(placeholders, ", ")), nil
	}

	// CONTAINS — case-insensitive substring (SQLite LIKE is case-insensitive for ASCII)
	if upper == "CONTAINS" {
		p.consume()
		v, err := p.parseValue()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s LIKE %s", toSQL(field), p.arg("%"+fmt.Sprint(v)+"%")), nil
	}

	// MATCHES / MATCHES* — regexp via registered REGEXP function
	if upper == "MATCHES" || upper == "MATCHES*" {
		p.consume()
		v, err := p.parseValue()
		if err != nil {
			return "", err
		}
		pattern := fmt.Sprint(v)
		if upper == "MATCHES*" {
			pattern = "(?i)" + pattern
		}
		return fmt.Sprintf("%s REGEXP %s", toSQL(field), p.arg(pattern)), nil
	}

	// Comparison operators
	if t.kind == tokOp || upper == "!=" {
		op := t.val
		p.consume()
		v, err := p.parseValue()
		if err != nil {
			return "", err
		}
		// Relative time: field > 10m  →  field > cutoff (passed as arg)
		if isTimeField(field) && (op == ">" || op == "<") {
			if dur, ok := parseDuration(fmt.Sprint(v)); ok {
				cutoff := time.Now().Add(-dur).UTC().Format(time.RFC3339Nano)
				return fmt.Sprintf("COALESCE(%s, ingested_at) %s %s",
					toSQL(field), op, p.arg(cutoff)), nil
			}
		}
		return fmt.Sprintf("%s %s %s", toSQL(field), op, p.arg(v)), nil
	}

	return "", fmt.Errorf("unknown operator %q", t.val)
}

func (p *parser) parseField() (string, error) {
	t := p.consume()
	if t.kind != tokWord {
		return "", fmt.Errorf("expected field name, got %q", t.val)
	}
	return t.val, nil
}

func (p *parser) parseValue() (any, error) {
	t := p.consume()
	switch t.kind {
	case tokString:
		return t.val, nil
	case tokWord:
		return t.val, nil
	}
	return nil, fmt.Errorf("expected value, got %q", t.val)
}

// toSQL maps a filter field to a SQL expression.
func toSQL(field string) string {
	if strings.HasPrefix(field, "data.") {
		key := strings.ReplaceAll(field[5:], "'", "''")
		return fmt.Sprintf("json_extract(data, '$.%s')", key)
	}
	switch field {
	case "log_time", "ingested_at", "source", "level", "message", "id":
		return field
	default:
		key := strings.ReplaceAll(field, "'", "''")
		return fmt.Sprintf("json_extract(data, '$.%s')", key)
	}
}

func isTimeField(field string) bool {
	return field == "log_time" || field == "ingested_at"
}

func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	num, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, false
	}
	switch unit {
	case 's':
		return time.Duration(num * float64(time.Second)), true
	case 'm':
		return time.Duration(num * float64(time.Minute)), true
	case 'h':
		return time.Duration(num * float64(time.Hour)), true
	case 'd':
		return time.Duration(num * float64(24*time.Hour)), true
	}
	return 0, false
}
