package main

// evalcmd.go — .eval command: safe-ish expression evaluator (owner only)
//
// Supports: arithmetic, string ops, basic Go expressions via yaegi-like
// limited interpreter. Since we can't add new deps easily, we implement
// a simple math + string evaluator.
//
// Usage:
//   .eval 1+2*3         → 7
//   .eval "hello"+" "+ "world"  → "hello world"
//   .eval len("test")   → 4
//   .eval 10/3          → 3
//   .eval 10.0/3.0      → 3.3333...
//   .eval help          → bantuan

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// Batas keamanan evaluator (anti-DoS):
const (
	maxEvalInputLen = 500 // panjang input maksimum (karakter)
	maxEvalDepth    = 50  // kedalaman rekursi maksimum
)

func handleEvalCommand(args string, msg *events.Message) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa pakai .eval"
	}

	expr := strings.TrimSpace(args)

	if expr == "" || expr == "help" || expr == "bantuan" {
		return `🧮 *EVAL — Expression Evaluator*

• *.eval <expr>* — evaluasi ekspresi

Support:
• Aritmatika: + - * / % ()
• Float: 10.0/3.0
• String: "hello" + " " + "world"
• len("text") → angka
• upper("text") / lower("TEXT")
• abs(-5) → 5
• max(1,2,3) / min(1,2,3)
• sqrt(16) → 4
• pow(2,10) → 1024
• now() → timestamp
• Contoh: *.eval 2+3*4*`
	}

	if len(expr) > maxEvalInputLen {
		return fmt.Sprintf("❌ Ekspresi terlalu panjang (max %d karakter)", maxEvalInputLen)
	}

	// Eval the expression
	result, err := evalExpression(expr)
	if err != nil {
		return fmt.Sprintf("❌ Error: %v", err)
	}
	return fmt.Sprintf("🧮 %s = `%v`", expr, result)
}

// evalExpression is a simple recursive descent parser for math + string expr.
// Grammar:
//
//	expr   = term (('+' | '-') term)*
//	term   = factor (('*' | '/' | '%') factor)*
//	factor = number | string | func(args) | '(' expr ')'
type evalParser struct {
	tokens []string
	pos    int
	depth  int // kedalaman rekursi saat ini (anti stack overflow)
}

// enter menambah kedalaman rekursi; error jika melebihi maxEvalDepth.
func (p *evalParser) enter() error {
	p.depth++
	if p.depth > maxEvalDepth {
		p.depth--
		return fmt.Errorf("ekspresi terlalu dalam (max %d level)", maxEvalDepth)
	}
	return nil
}

func (p *evalParser) leave() {
	p.depth--
}

func evalExpression(input string) (interface{}, error) {
	tokens, err := evalTokenize(input)
	if err != nil {
		return nil, err
	}
	p := &evalParser{tokens: tokens}
	result, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.tokens) {
		return nil, fmt.Errorf("unexpected token: %s", p.tokens[p.pos])
	}
	return result, nil
}

func evalTokenize(input string) ([]string, error) {
	var tokens []string
	i := 0
	for i < len(input) {
		c := input[i]
		// Skip whitespace
		if c == ' ' || c == '\t' || c == '\n' {
			i++
			continue
		}
		// String literal
		if c == '"' || c == '\'' {
			quote := c
			i++
			start := i
			for i < len(input) && input[i] != quote {
				i++
			}
			if i >= len(input) {
				return nil, fmt.Errorf("unterminated string")
			}
			tokens = append(tokens, "\""+input[start:i]+"\"")
			i++
			continue
		}
		// Number (int or float)
		if (c >= '0' && c <= '9') || c == '.' {
			start := i
			for i < len(input) && ((input[i] >= '0' && input[i] <= '9') || input[i] == '.') {
				i++
			}
			tokens = append(tokens, input[start:i])
			continue
		}
		// Identifier (function name)
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			start := i
			for i < len(input) && ((input[i] >= 'a' && input[i] <= 'z') ||
				(input[i] >= 'A' && input[i] <= 'Z') ||
				(input[i] >= '0' && input[i] <= '9') || input[i] == '_') {
				i++
			}
			tokens = append(tokens, input[start:i])
			continue
		}
		// Operators + parens + comma
		if strings.ContainsRune("+-*/%(),", rune(c)) {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		return nil, fmt.Errorf("unexpected char: %c", c)
	}
	return tokens, nil
}

func (p *evalParser) parseExpr() (interface{}, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for p.pos < len(p.tokens) && (p.tokens[p.pos] == "+" || p.tokens[p.pos] == "-") {
		op := p.tokens[p.pos]
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		left = evalApplyOp(op, left, right)
	}
	return left, nil
}

func (p *evalParser) parseTerm() (interface{}, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for p.pos < len(p.tokens) && (p.tokens[p.pos] == "*" || p.tokens[p.pos] == "/" || p.tokens[p.pos] == "%") {
		op := p.tokens[p.pos]
		p.pos++
		right, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		left = evalApplyOp(op, left, right)
	}
	return left, nil
}

func (p *evalParser) parseFactor() (interface{}, error) {
	if p.pos >= len(p.tokens) {
		return nil, fmt.Errorf("unexpected end")
	}
	tok := p.tokens[p.pos]

	// Parens
	if tok == "(" {
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		p.pos++
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.tokens) || p.tokens[p.pos] != ")" {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return val, nil
	}

	// Unary minus
	if tok == "-" {
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		p.pos++
		val, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return evalApplyOp("*", -1, val), nil
	}

	// Number
	if num, ok := evalParseNumber(tok); ok {
		p.pos++
		return num, nil
	}

	// String literal
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		p.pos++
		return tok[1 : len(tok)-1], nil
	}

	// Function call: name(args)
	if p.pos+1 < len(p.tokens) && p.tokens[p.pos+1] == "(" {
		name := tok
		p.pos += 2 // skip name + (
		var args []interface{}
		if p.tokens[p.pos] != ")" {
			for {
				if err := p.enter(); err != nil {
					return nil, err
				}
				val, err := p.parseExpr()
				p.leave()
				if err != nil {
					return nil, err
				}
				args = append(args, val)
				if p.pos < len(p.tokens) && p.tokens[p.pos] == "," {
					p.pos++
					continue
				}
				break
			}
		}
		if p.pos >= len(p.tokens) || p.tokens[p.pos] != ")" {
			return nil, fmt.Errorf("missing ) after args")
		}
		p.pos++
		return evalCallFunc(name, args)
	}

	// Bare identifier — treat as string
	p.pos++
	return tok, nil
}

func evalParseNumber(s string) (interface{}, bool) {
	if strings.Contains(s, ".") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, false
		}
		return f, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, false
	}
	return n, true
}

// evalCheckedInt64 melakukan op aritmatika int64 dengan deteksi overflow.
// Mengembalikan (hasil, true) jika aman, atau (0, false) jika overflow.
func evalCheckedInt64(op string, a, b int64) (int64, bool) {
	switch op {
	case "+":
		r := a + b
		if (r > a) != (b > 0) {
			return 0, false
		}
		return r, true
	case "-":
		r := a - b
		if (r < a) != (b > 0) {
			return 0, false
		}
		return r, true
	case "*":
		if a == 0 || b == 0 {
			return 0, true
		}
		// Kasus khusus: MinInt64 * -1 overflow tapi lolos cek r/b != a
		// (di Go, MinInt64/-1 == MinInt64 tanpa panic).
		if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
			return 0, false
		}
		r := a * b
		if r/b != a {
			return 0, false
		}
		return r, true
	}
	return 0, false
}

func evalApplyOp(op string, left, right interface{}) interface{} {
	// String concat
	if op == "+" {
		if ls, lok := left.(string); lok {
			if rs, rok := right.(string); rok {
				return ls + rs
			}
			return ls + fmt.Sprintf("%v", right)
		}
		if rs, rok := right.(string); rok {
			return fmt.Sprintf("%v", left) + rs
		}
	}
	// Numeric ops
	lf, rf := evalToFloat(left), evalToFloat(right)
	switch op {
	case "+":
		if evalIsInt(left) && evalIsInt(right) {
			if r, ok := evalCheckedInt64("+", left.(int64), right.(int64)); ok {
				return r
			}
			return "⚠️ integer overflow"
		}
		return lf + rf
	case "-":
		if evalIsInt(left) && evalIsInt(right) {
			if r, ok := evalCheckedInt64("-", left.(int64), right.(int64)); ok {
				return r
			}
			return "⚠️ integer overflow"
		}
		return lf - rf
	case "*":
		if evalIsInt(left) && evalIsInt(right) {
			if r, ok := evalCheckedInt64("*", left.(int64), right.(int64)); ok {
				return r
			}
			return "⚠️ integer overflow"
		}
		return lf * rf
	case "/":
		if rf == 0 {
			return "∞"
		}
		if evalIsInt(left) && evalIsInt(right) {
			return left.(int64) / right.(int64)
		}
		return lf / rf
	case "%":
		if evalIsInt(left) && evalIsInt(right) {
			if right.(int64) == 0 {
				return "∞"
			}
			return left.(int64) % right.(int64)
		}
		return int64(lf) % int64(rf)
	}
	return nil
}

func evalIsInt(v interface{}) bool {
	_, ok := v.(int64)
	return ok
}

func evalToFloat(v interface{}) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

func evalCallFunc(name string, args []interface{}) (interface{}, error) {
	switch name {
	case "len":
		if len(args) != 1 {
			return nil, fmt.Errorf("len() needs 1 arg")
		}
		if s, ok := args[0].(string); ok {
			return int64(len(s)), nil
		}
		return nil, fmt.Errorf("len() needs string")
	case "upper":
		if len(args) != 1 {
			return nil, fmt.Errorf("upper() needs 1 arg")
		}
		return strings.ToUpper(fmt.Sprintf("%v", args[0])), nil
	case "lower":
		if len(args) != 1 {
			return nil, fmt.Errorf("lower() needs 1 arg")
		}
		return strings.ToLower(fmt.Sprintf("%v", args[0])), nil
	case "abs":
		if len(args) != 1 {
			return nil, fmt.Errorf("abs() needs 1 arg")
		}
		f := evalToFloat(args[0])
		if f < 0 {
			return -f, nil
		}
		return args[0], nil
	case "max":
		if len(args) < 2 {
			return nil, fmt.Errorf("max() needs 2+ args")
		}
		var best float64
		first := true
		for _, a := range args {
			f := evalToFloat(a)
			if first || f > best {
				best = f
				first = false
			}
		}
		return best, nil
	case "min":
		if len(args) < 2 {
			return nil, fmt.Errorf("min() needs 2+ args")
		}
		var best float64
		first := true
		for _, a := range args {
			f := evalToFloat(a)
			if first || f < best {
				best = f
				first = false
			}
		}
		return best, nil
	case "now":
		return int64(time.Now().Unix()), nil
	case "sqrt":
		if len(args) != 1 {
			return nil, fmt.Errorf("sqrt() needs 1 arg")
		}
		f := evalToFloat(args[0])
		if f < 0 {
			return nil, fmt.Errorf("sqrt() of negative number")
		}
		return math.Sqrt(f), nil
	case "pow":
		if len(args) != 2 {
			return nil, fmt.Errorf("pow() needs 2 args")
		}
		return math.Pow(evalToFloat(args[0]), evalToFloat(args[1])), nil
	}
	return nil, fmt.Errorf("unknown function: %s", name)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "eval",
		Description: "eval",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleEvalCommand(args, msg)
		},
	})
}
