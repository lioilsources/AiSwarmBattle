package preprocess

import (
	"strings"
	"testing"
)

func TestFlutter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "flutter include on first line",
			in:   "#include <flutter/runtime_effect.glsl>\nout vec4 fragColor;\nvoid main() { fragColor = FlutterFragCoord(); }",
			want: "#version 300 es\nprecision highp float;\n#line 2\nout vec4 fragColor;\nvoid main() { fragColor = gl_FragCoord; }",
		},
		{
			name: "include after leading comment keeps numbering",
			in:   "// header\n#include <flutter/runtime_effect.glsl>\nvoid main() { vec2 p = FlutterFragCoord().xy; }",
			want: "// header\n#version 300 es\nprecision highp float;\n#line 3\nvoid main() { vec2 p = gl_FragCoord.xy; }",
		},
		{
			name: "no include, no version",
			in:   "out vec4 c;\nvoid main() { c = vec4(1.0); }",
			want: "#version 300 es\nprecision highp float;\n#line 1\nout vec4 c;\nvoid main() { c = vec4(1.0); }",
		},
		{
			name: "explicit version is kept",
			in:   "#version 300 es\nprecision mediump float;\nvoid main() { vec4 p = FlutterFragCoord( ); }",
			want: "#version 300 es\nprecision mediump float;\nvoid main() { vec4 p = gl_FragCoord; }",
		},
		{
			name: "crlf normalized",
			in:   "#include <flutter/runtime_effect.glsl>\r\nvoid main() {}\r\n",
			want: "#version 300 es\nprecision highp float;\n#line 2\nvoid main() {}\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Flutter(tc.in).Source
			if got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestFlutterDeterministic(t *testing.T) {
	in := "#include <flutter/runtime_effect.glsl>\nvoid main() { vec2 p = FlutterFragCoord().xy; }"
	a, b := Flutter(in), Flutter(in)
	if a.Source != b.Source || strings.Join(a.Notes, "|") != strings.Join(b.Notes, "|") {
		t.Fatal("preprocess is not deterministic")
	}
}
