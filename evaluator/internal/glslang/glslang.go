// Package glslang validates GLSL ES 3.0 fragment shaders and emits SPIR-V by
// executing the Khronos glslangValidator binary.
//
// TODO: replace exec with the cgo C interface once the ARM64 build image ships
// libglslang; the Compile signature is meant to stay the same.
package glslang

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/lioilsources/shaderbattle/pkg/types"
)

// Compiler wraps a glslangValidator executable.
type Compiler struct {
	Bin string
}

// Result is the outcome of a compile. Errors and Warnings refer to lines of
// the source passed to Compile (after #line remapping).
type Result struct {
	OK       bool
	SPIRV    []byte
	Errors   []types.Diagnostic
	Warnings []types.Diagnostic
}

var (
	versionLineRe = regexp.MustCompile(`(?m)^\s*#\s*version\s+300\s+es\s*$`)
	diagLocRe     = regexp.MustCompile(`^(ERROR|WARNING): \d+:(\d+):(?:(\d+):)? (.*)$`)
	diagRe        = regexp.MustCompile(`^(ERROR|WARNING): (.*)$`)
	summaryRe     = regexp.MustCompile(`^\d+ compilation errors\.`)
	glslangVerRe  = regexp.MustCompile(`Glslang Version: \d+:(\S+)`)
)

// Compile validates src as a GLSL ES 3.0 fragment shader. A shader that fails
// to compile is reported via Result.OK=false; a non-nil error means the tool
// itself could not run.
//
// SPIR-V cannot be generated from ES 300 sources, so after a successful
// validation the shader is retargeted to "#version 310 es" and compiled with
// OpenGL semantics (-G) to produce the binary.
func (c *Compiler) Compile(ctx context.Context, src string) (*Result, error) {
	dir, err := os.MkdirTemp("", "evaluator-glslang-")
	if err != nil {
		return nil, fmt.Errorf("glslang: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "shader.frag")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		return nil, fmt.Errorf("glslang: write source: %w", err)
	}

	out, ok, err := c.run(ctx, "--error-column", path)
	if err != nil {
		return nil, err
	}
	res := &Result{OK: ok}
	res.Errors, res.Warnings = parseDiagnostics(out)
	if !ok {
		if len(res.Errors) == 0 {
			res.Errors = []types.Diagnostic{{Severity: "error", Message: strings.TrimSpace(out)}}
		}
		return res, nil
	}

	spirvSrc := src
	if versionLineRe.MatchString(src) {
		spirvSrc = versionLineRe.ReplaceAllString(src, "#version 310 es")
	}
	spirvPath := filepath.Join(dir, "spirv.frag")
	if err := os.WriteFile(spirvPath, []byte(spirvSrc), 0o600); err != nil {
		return nil, fmt.Errorf("glslang: write spirv source: %w", err)
	}
	spv := filepath.Join(dir, "shader.spv")
	out, ok, err = c.run(ctx, "-G", "--amb", "--aml", "-o", spv, spirvPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		res.Warnings = append(res.Warnings, types.Diagnostic{
			Severity: "warn",
			Message:  "spirv generation failed: " + firstLine(out),
		})
		return res, nil
	}
	res.SPIRV, err = os.ReadFile(spv)
	if err != nil {
		return nil, fmt.Errorf("glslang: read spirv: %w", err)
	}
	return res, nil
}

// Version returns the glslang version string, e.g. "16.6.0".
func (c *Compiler) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, c.Bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("glslang: version: %w", err)
	}
	m := glslangVerRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("glslang: unrecognized version output: %q", firstLine(string(out)))
	}
	return string(m[1]), nil
}

// run executes glslangValidator. ok reports compile success (exit 0); exit 2
// is a compile failure. Any other failure is returned as error.
func (c *Compiler) run(ctx context.Context, args ...string) (string, bool, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return buf.String(), true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 && ctx.Err() == nil {
		return buf.String(), false, nil
	}
	return "", false, fmt.Errorf("glslang: run %s: %w: %s", c.Bin, err, firstLine(buf.String()))
}

func parseDiagnostics(out string) (errs, warns []types.Diagnostic) {
	errs, warns = []types.Diagnostic{}, []types.Diagnostic{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		var d types.Diagnostic
		var kind string
		if m := diagLocRe.FindStringSubmatch(line); m != nil {
			kind = m[1]
			d.Line, _ = strconv.Atoi(m[2])
			if m[3] != "" {
				d.Column, _ = strconv.Atoi(m[3])
			}
			d.Message = strings.TrimSpace(m[4])
		} else if m := diagRe.FindStringSubmatch(line); m != nil {
			kind = m[1]
			d.Message = strings.TrimSpace(m[2])
		} else {
			continue
		}
		if summaryRe.MatchString(d.Message) || d.Message == "'' : compilation terminated" {
			continue
		}
		if kind == "ERROR" {
			d.Severity = "error"
			errs = append(errs, d)
		} else {
			d.Severity = "warn"
			warns = append(warns, d)
		}
	}
	return errs, warns
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
