// Package static computes deterministic source metrics and an ALU cost
// estimate for a GLSL ES 3.0 fragment shader.
//
// The analysis is token-level (no AST). Known limitations, documented in
// docs/scoring_rubric.md:
//   - user function bodies are costed once, calls are not inlined;
//   - vector width is ignored (vec4 * vec4 costs the same as float * float);
//   - unary minus is counted as a subtraction.
//
// TODO: switch to tree-sitter-glsl when the token-level estimate is not
// precise enough.
package static

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/lioilsources/shaderbattle/pkg/types"
)

// Weights is the per-operation cost table. Values follow EVALUATOR.md.
type Weights struct {
	AddSubMul      float64
	Div            float64
	SqrtRsqrt      float64
	ExpLog         float64
	SinCosTan      float64
	Pow            float64
	TextureRead    float64
	DiscardPenalty float64
	BranchPenalty  float64
	// UnknownLoopIter is the multiplier for loops whose bound is not a
	// compile-time constant.
	UnknownLoopIter float64
}

// DefaultWeights is the cost table of the current cost model version.
var DefaultWeights = Weights{
	AddSubMul:       1,
	Div:             4,
	SqrtRsqrt:       4,
	ExpLog:          4,
	SinCosTan:       8,
	Pow:             8,
	TextureRead:     100,
	DiscardPenalty:  50,
	BranchPenalty:   2,
	UnknownLoopIter: 16,
}

var (
	textureFuncs = set("texture", "textureLod", "textureProj", "textureProjLod", "textureGrad",
		"textureOffset", "textureLodOffset", "textureGradOffset", "texelFetch", "texelFetchOffset",
		"texture2D", "textureCube")
	trigFuncs    = set("sin", "cos", "tan", "asin", "acos", "atan", "sinh", "cosh", "tanh")
	expLogFuncs  = set("exp", "exp2", "log", "log2")
	sqrtFuncs    = set("sqrt", "inversesqrt", "length", "normalize", "distance")
	derivFuncs   = set("dFdx", "dFdy", "fwidth")
	precisionKws = set("highp", "mediump", "lowp")

	defineRe   = regexp.MustCompile(`(?m)^\s*#\s*define\s+(\w+)\s+\(?\s*([-+]?[0-9.]+)\s*\)?\s*$`)
	constRe    = regexp.MustCompile(`\bconst\s+(?:(?:highp|mediump|lowp)\s+)?(?:int|uint|float)\s+(\w+)\s*=\s*([-+]?[0-9.]+)[uU]?\s*;`)
	precDeclRe = regexp.MustCompile(`\bprecision\s+(highp|mediump|lowp)\s+float\s*;`)
)

// Analyze computes metrics for preprocessed GLSL ES source.
func Analyze(src string, w Weights) types.StaticResult {
	clean := stripComments(src)
	consts := constants(clean)
	toks := tokenize(stripPreprocessor(clean))

	r := types.StaticResult{
		PrecisionDefault: "none",
	}
	if m := precDeclRe.FindStringSubmatch(clean); m != nil {
		r.PrecisionDefault = m[1]
	}
	r.LOC, r.LOCLogical = countLines(src, clean)

	a := analyzer{toks: toks, w: w, consts: consts, res: &r}
	a.run()
	r.EstimatedALU = int(math.Round(a.cost))
	return r
}

type token struct {
	text  string
	ident bool
}

type scope struct {
	mult      float64
	braceOpen int // brace depth at which the scope ends; -1 for single-statement bodies
	parenOpen int
}

type analyzer struct {
	toks   []token
	w      Weights
	consts map[string]float64
	res    *types.StaticResult
	cost   float64
}

func (a *analyzer) run() {
	depth, paren := 0, 0
	scopes := []scope{}
	knownMax := 0
	unknown := false
	pending := 0.0 // multiplier of a loop whose header was just parsed

	mult := func() float64 {
		m := 1.0
		for _, s := range scopes {
			m *= s.mult
		}
		return m
	}

	for i := 0; i < len(a.toks); i++ {
		t := a.toks[i]
		next := ""
		if i+1 < len(a.toks) {
			next = a.toks[i+1].text
		}

		if pending != 0 {
			if t.text == "{" {
				scopes = append(scopes, scope{mult: pending, braceOpen: depth})
			} else {
				scopes = append(scopes, scope{mult: pending, braceOpen: -1, parenOpen: paren})
			}
			pending = 0
		}

		switch {
		case t.text == "{":
			depth++
		case t.text == "}":
			depth--
			for len(scopes) > 0 && scopes[len(scopes)-1].braceOpen == depth {
				scopes = scopes[:len(scopes)-1]
			}
		case t.text == "(":
			paren++
		case t.text == ")":
			paren--
			if depth == 0 && paren == 0 && next == "{" {
				a.res.FunctionCount++
			}
		case t.text == ";":
			for len(scopes) > 0 && scopes[len(scopes)-1].braceOpen == -1 && scopes[len(scopes)-1].parenOpen == paren {
				scopes = scopes[:len(scopes)-1]
			}
		}

		m := mult()
		switch {
		case t.text == "for":
			a.res.LoopCount++
			iter, end, ok := a.forIterations(i)
			if ok {
				knownMax = max(knownMax, iter)
				pending = float64(iter)
			} else {
				unknown = true
				pending = a.w.UnknownLoopIter
			}
			// Cost the loop header once per iteration as part of the body.
			i = end
			paren = 0
			continue
		case t.text == "while":
			if i > 0 && a.toks[i-1].text == "}" && a.isDoWhileTail(i) {
				continue
			}
			a.res.LoopCount++
			unknown = true
			end := a.matchParen(i + 1)
			pending = a.w.UnknownLoopIter
			i = end
			continue
		case t.text == "do":
			a.res.LoopCount++
			unknown = true
			pending = a.w.UnknownLoopIter
		case t.text == "if" || t.text == "?":
			a.res.BranchCount++
			a.cost += a.w.BranchPenalty * m
		case t.text == "discard":
			a.res.UsesDiscard = true
			a.cost += a.w.DiscardPenalty * m
		case t.text == "uniform":
			a.res.UniformCount += a.declCount(i)
		case t.ident && next == "(":
			switch {
			case textureFuncs[t.text]:
				a.res.TexReads++
				a.cost += a.w.TextureRead * m
			case trigFuncs[t.text]:
				a.cost += a.w.SinCosTan * m
			case t.text == "pow":
				a.cost += a.w.Pow * m
			case expLogFuncs[t.text]:
				a.cost += a.w.ExpLog * m
			case sqrtFuncs[t.text]:
				a.cost += a.w.SqrtRsqrt * m
			case derivFuncs[t.text]:
				a.res.UsesDerivatives = true
			}
		case t.text == "+" || t.text == "-" || t.text == "*" || t.text == "+=" || t.text == "-=" || t.text == "*=":
			a.cost += a.w.AddSubMul * m
		case t.text == "/" || t.text == "/=":
			a.cost += a.w.Div * m
		}
	}

	if unknown {
		a.res.LoopMaxIter = -1
	} else {
		a.res.LoopMaxIter = knownMax
	}
}

// forIterations parses "for ( [type] i = A ; i op B ; step )" starting at the
// "for" token. It returns the iteration count, the index of the closing paren
// and whether the count is statically known.
func (a *analyzer) forIterations(i int) (int, int, bool) {
	end := a.matchParen(i + 1)
	if end < 0 {
		return 0, len(a.toks) - 1, false
	}
	parts := splitTokens(a.toks[i+2:end], ";")
	if len(parts) != 3 {
		return 0, end, false
	}
	init, cond, step := parts[0], parts[1], parts[2]

	// init: [qualifiers] [type] name = value
	eq := indexOf(init, "=")
	if eq < 1 || eq+1 >= len(init) {
		return 0, end, false
	}
	name := init[eq-1].text
	start, ok := a.value(init[eq+1:])
	if !ok {
		return 0, end, false
	}

	// cond: name op value
	if len(cond) < 3 || cond[0].text != name {
		return 0, end, false
	}
	op := cond[1].text
	limit, ok := a.value(cond[2:])
	if !ok {
		return 0, end, false
	}

	var inc float64
	switch {
	case len(step) == 2 && (step[0].text == "++" || step[1].text == "++"):
		inc = 1
	case len(step) == 2 && (step[0].text == "--" || step[1].text == "--"):
		inc = -1
	case len(step) >= 3 && step[0].text == name && step[1].text == "+=":
		v, ok := a.value(step[2:])
		if !ok {
			return 0, end, false
		}
		inc = v
	case len(step) >= 3 && step[0].text == name && step[1].text == "-=":
		v, ok := a.value(step[2:])
		if !ok {
			return 0, end, false
		}
		inc = -v
	default:
		return 0, end, false
	}
	if inc == 0 {
		return 0, end, false
	}

	var n float64
	switch op {
	case "<":
		n = math.Ceil((limit - start) / inc)
	case "<=":
		n = math.Floor((limit-start)/inc) + 1
	case ">":
		n = math.Ceil((start - limit) / -inc)
	case ">=":
		n = math.Floor((start-limit)/-inc) + 1
	case "!=":
		n = (limit - start) / inc
		if n != math.Trunc(n) {
			return 0, end, false
		}
	default:
		return 0, end, false
	}
	if n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, end, false
	}
	return int(n), end, true
}

// value evaluates a literal or a named constant, optionally wrapped in a
// constructor or cast such as float(N) or int(N).
func (a *analyzer) value(ts []token) (float64, bool) {
	for len(ts) >= 3 && ts[1].text == "(" && ts[len(ts)-1].text == ")" {
		ts = ts[2 : len(ts)-1]
	}
	neg := false
	if len(ts) == 2 && ts[0].text == "-" {
		neg, ts = true, ts[1:]
	}
	if len(ts) != 1 {
		return 0, false
	}
	v, ok := a.consts[ts[0].text]
	if !ok {
		f, err := strconv.ParseFloat(strings.TrimRight(ts[0].text, "uUfF"), 64)
		if err != nil {
			return 0, false
		}
		v = f
	}
	if neg {
		v = -v
	}
	return v, true
}

func (a *analyzer) matchParen(i int) int {
	if i >= len(a.toks) || a.toks[i].text != "(" {
		return -1
	}
	d := 0
	for j := i; j < len(a.toks); j++ {
		switch a.toks[j].text {
		case "(":
			d++
		case ")":
			d--
			if d == 0 {
				return j
			}
		}
	}
	return -1
}

// isDoWhileTail reports whether the "while" at i closes a do { } block.
func (a *analyzer) isDoWhileTail(i int) bool {
	end := a.matchParen(i + 1)
	return end > 0 && end+1 < len(a.toks) && a.toks[end+1].text == ";"
}

// declCount counts declarators in "uniform type a, b, c;".
func (a *analyzer) declCount(i int) int {
	n := 1
	for j := i + 1; j < len(a.toks) && a.toks[j].text != ";"; j++ {
		switch a.toks[j].text {
		case ",":
			n++
		case "{":
			return 1 // uniform block counts as one
		}
	}
	return n
}

func splitTokens(ts []token, sep string) [][]token {
	var out [][]token
	d, last := 0, 0
	for i, t := range ts {
		switch t.text {
		case "(":
			d++
		case ")":
			d--
		case sep:
			if d == 0 {
				out = append(out, ts[last:i])
				last = i + 1
			}
		}
	}
	return append(out, ts[last:])
}

func indexOf(ts []token, s string) int {
	for i, t := range ts {
		if t.text == s {
			return i
		}
	}
	return -1
}

func constants(src string) map[string]float64 {
	out := map[string]float64{}
	for _, m := range defineRe.FindAllStringSubmatch(src, -1) {
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			out[m[1]] = v
		}
	}
	for _, m := range constRe.FindAllStringSubmatch(src, -1) {
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			out[m[1]] = v
		}
	}
	return out
}

// stripComments removes // and /* */ comments, keeping newlines.
func stripComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); i++ {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				b.WriteByte('\n')
			}
		case strings.HasPrefix(src[i:], "/*"):
			i += 2
			for i < len(src) && !strings.HasPrefix(src[i:], "*/") {
				if src[i] == '\n' {
					b.WriteByte('\n')
				}
				i++
			}
			i++
		default:
			b.WriteByte(src[i])
		}
	}
	return b.String()
}

func stripPreprocessor(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// countLines returns non-blank source lines and logical lines (non-blank,
// non-comment, non-preprocessor, not brace-only).
func countLines(src, clean string) (loc, logical int) {
	for _, l := range strings.Split(src, "\n") {
		if strings.TrimSpace(l) != "" {
			loc++
		}
	}
	for _, l := range strings.Split(clean, "\n") {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, "#") || strings.Trim(s, "{}; ") == "" {
			continue
		}
		logical++
	}
	return loc, logical
}

var multiOps = []string{"++", "--", "+=", "-=", "*=", "/=", "<=", ">=", "==", "!=", "&&", "||", "<<", ">>"}

func tokenize(src string) []token {
	var out []token
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			out = append(out, token{text: src[i:j], ident: true})
			i = j
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			j := i + 1
			for j < len(src) && (isIdentPart(src[j]) || src[j] == '.' ||
				((src[j] == '+' || src[j] == '-') && (src[j-1] == 'e' || src[j-1] == 'E'))) {
				j++
			}
			out = append(out, token{text: src[i:j]})
			i = j
		default:
			op := string(c)
			for _, m := range multiOps {
				if strings.HasPrefix(src[i:], m) {
					op = m
					break
				}
			}
			out = append(out, token{text: op})
			i += len(op)
		}
	}
	return out
}

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
