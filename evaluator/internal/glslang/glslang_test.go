package glslang

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lioilsources/shaderbattle/evaluator/internal/preprocess"
	"github.com/lioilsources/shaderbattle/pkg/types"
)

func compiler(t *testing.T) *Compiler {
	t.Helper()
	bin := os.Getenv("GLSLANG_VALIDATOR")
	if bin == "" {
		bin = "glslangValidator"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("glslangValidator not available: %v", err)
	}
	return &Compiler{Bin: bin}
}

func TestCompileReferenceShaders(t *testing.T) {
	c := compiler(t)
	files, err := filepath.Glob("../../testdata/*.frag")
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata shaders: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.Compile(context.Background(), preprocess.Flutter(string(src)).Source)
			if err != nil {
				t.Fatal(err)
			}
			if !res.OK {
				t.Fatalf("compile failed: %+v", res.Errors)
			}
			if len(res.SPIRV) < 20 || res.SPIRV[0] != 0x03 || res.SPIRV[3] != 0x07 {
				t.Fatalf("missing or invalid SPIR-V (%d bytes)", len(res.SPIRV))
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	c := compiler(t)
	tests := []struct {
		name     string
		src      string
		wantLine int
	}{
		{
			name:     "undeclared identifier keeps original line",
			src:      "#include <flutter/runtime_effect.glsl>\nout vec4 fragColor;\nvoid main() {\n  fragColor = vec4(missing);\n}",
			wantLine: 4,
		},
		{
			name:     "type mismatch",
			src:      "#include <flutter/runtime_effect.glsl>\nout vec4 fragColor;\nvoid main() { float a = 1; fragColor = vec4(a); }",
			wantLine: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Compile(context.Background(), preprocess.Flutter(tc.src).Source)
			if err != nil {
				t.Fatal(err)
			}
			if res.OK {
				t.Fatal("expected compile failure")
			}
			if len(res.Errors) == 0 || res.Errors[0].Line != tc.wantLine {
				t.Fatalf("want first error on line %d, got %+v", tc.wantLine, res.Errors)
			}
			if res.SPIRV != nil {
				t.Fatal("SPIR-V must be empty on failure")
			}
		})
	}
}

func TestParseDiagnostics(t *testing.T) {
	out := "shader.frag\n" +
		"ERROR: 0:4:21: 'missing' : undeclared identifier \n" +
		"ERROR: 0:4:22: '' : compilation terminated \n" +
		"WARNING: 0:7: 'foo' : something odd\n" +
		"ERROR: #version: bad\n" +
		"ERROR: 3 compilation errors.  No code generated.\n"
	errs, warns := parseDiagnostics(out)
	wantErrs := []types.Diagnostic{
		{Severity: "error", Line: 4, Column: 21, Message: "'missing' : undeclared identifier"},
		{Severity: "error", Message: "#version: bad"},
	}
	if len(errs) != len(wantErrs) {
		t.Fatalf("errors: got %+v", errs)
	}
	for i := range wantErrs {
		if errs[i] != wantErrs[i] {
			t.Errorf("error %d: got %+v want %+v", i, errs[i], wantErrs[i])
		}
	}
	if len(warns) != 1 || warns[0].Line != 7 || warns[0].Severity != "warn" {
		t.Errorf("warnings: got %+v", warns)
	}
}
