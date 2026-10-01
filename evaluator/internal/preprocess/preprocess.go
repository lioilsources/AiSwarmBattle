// Package preprocess turns Flutter/Impeller runtime-effect GLSL into plain
// GLSL ES 3.0 that glslang and a GLES 3.0 driver accept.
//
// The transformation is deterministic and line-preserving: diagnostics from
// the compiler refer to line numbers of the original Flutter source.
package preprocess

import (
	"fmt"
	"regexp"
	"strings"
)

// Version identifies the preprocessing rules. Bump it on any change to the
// transformation; it is reported alongside the cost model version.
const Version = "flutter-es300.1"

const flutterInclude = "#include <flutter/runtime_effect.glsl>"

// header replaces the Flutter include. Flutter injects #version itself and
// Impeller compiles with highp float, so both are made explicit here.
var header = []string{
	"#version 300 es",
	"precision highp float;",
}

var (
	versionRe     = regexp.MustCompile(`(?m)^\s*#\s*version\b`)
	fragCoordRe   = regexp.MustCompile(`\bFlutterFragCoord\s*\(\s*\)`)
	includeLineRe = regexp.MustCompile(`^\s*#\s*include\s*<flutter/runtime_effect\.glsl>\s*$`)
)

// Result is the preprocessed source plus a record of what was changed.
type Result struct {
	Source string
	Notes  []string
}

// Flutter converts Flutter runtime-effect GLSL into GLSL ES 3.0.
//
//   - the Flutter include is replaced by "#version 300 es" + default float
//     precision, followed by a #line directive that restores original numbering;
//   - FlutterFragCoord() becomes gl_FragCoord (both are vec4);
//   - sources that already declare #version are left as-is apart from the
//     FlutterFragCoord rewrite.
func Flutter(src string) Result {
	var notes []string
	src = strings.ReplaceAll(src, "\r\n", "\n")

	if n := len(fragCoordRe.FindAllStringIndex(src, -1)); n > 0 {
		src = fragCoordRe.ReplaceAllString(src, "gl_FragCoord")
		notes = append(notes, fmt.Sprintf("FlutterFragCoord() -> gl_FragCoord (%d)", n))
	}

	lines := strings.Split(src, "\n")
	hasVersion := versionRe.MatchString(src)

	for i, line := range lines {
		if !includeLineRe.MatchString(line) {
			continue
		}
		if hasVersion {
			lines[i] = ""
			notes = append(notes, "removed "+flutterInclude)
			return Result{Source: strings.Join(lines, "\n"), Notes: notes}
		}
		repl := append(append([]string{}, header...), fmt.Sprintf("#line %d", i+2))
		out := make([]string, 0, len(lines)+len(repl))
		out = append(out, lines[:i]...)
		out = append(out, repl...)
		out = append(out, lines[i+1:]...)
		notes = append(notes, "replaced "+flutterInclude+" with #version 300 es + precision highp float")
		return Result{Source: strings.Join(out, "\n"), Notes: notes}
	}

	if hasVersion {
		return Result{Source: src, Notes: notes}
	}
	out := append(append([]string{}, header...), "#line 1", src)
	notes = append(notes, "injected #version 300 es + precision highp float")
	return Result{Source: strings.Join(out, "\n"), Notes: notes}
}
