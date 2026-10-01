// Package egl profiles fragment shaders on a headless EGL + GLES 3.0 context.
//
// The GL context is owned by a single goroutine locked to its OS thread; all
// jobs are serialized through a channel (AGENTS.md: EGL is single-threaded).
package egl

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"
)

// ErrUnavailable is returned when the binary was built without EGL support or
// no EGL display could be initialized.
var ErrUnavailable = errors.New("egl: runtime profiling unavailable")

// CompileError is a shader the GLES driver rejected even though glslang
// accepted it.
type CompileError struct{ Log string }

func (e *CompileError) Error() string { return "egl: driver compile/link failed: " + e.Log }

const (
	// sampleSize is the edge of the downsampled readback used per frame.
	sampleSize = 128
	// constantLag is the frame distance used for constant-frame detection
	// (0.5 s at 60 fps): frame N is constant if identical to frame N-lag.
	constantLag = 30
	// blackEpsilon: a frame is black when every sampled channel is below it.
	blackEpsilon = 1.0 / 255.0
	// cameraSize is the edge of the deterministic camera test image.
	cameraSize = 512
	fps        = 60.0
)

// Options configure context creation.
type Options struct {
	// PreferSoftware selects a software (llvmpipe) EGL device first.
	PreferSoftware bool
}

// Info describes the initialized context.
type Info struct {
	Renderer string
	Version  string
	Software bool
}

// Uniform is a shader input. Value holds the components for float/vecN/int.
type Uniform struct {
	Name     string
	Type     string
	Value    []float64
	Semantic string // time|resolution|camera|""
}

// Job is one profiling run at one resolution.
type Job struct {
	Source   string // GLSL ES 3.0 fragment shader
	Uniforms []Uniform
	Width    int
	Height   int
	Frames   int
	Deadline time.Time // rendering stops early once reached
}

// Result holds per-job measurements.
type Result struct {
	FramesRendered int
	FirstFrameMS   float64
	MedianMS       float64
	P95MS          float64
	NaNInf         bool
	BlackFrames    int
	ConstantFrames int
	Animated       bool // a time uniform is active, constant detection applied
	Truncated      bool // deadline hit before all frames were rendered
}

// backend is the GL surface used by execute; implemented with cgo on linux.
type backend interface {
	info() Info
	useProgram(src string) error
	releaseProgram()
	location(name string) int32
	setUniform(loc int32, typ string, v []float32)
	bindCamera(loc int32, unit int32)
	setTarget(w, h int) error
	drawFrame()
	readSample(dst []float32)
	readFull(dst []float32)
	close()
}

type request struct {
	job   Job
	reply chan reply
}

type reply struct {
	res Result
	err error
}

// Renderer serializes profiling jobs onto the single EGL goroutine.
type Renderer struct {
	jobs chan request
	inf  Info
}

// New initializes the EGL context on a dedicated OS thread.
func New(opts Options) (*Renderer, error) {
	r := &Renderer{jobs: make(chan request)}
	initErr := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		b, err := newBackend(opts)
		if err != nil {
			initErr <- err
			return
		}
		defer b.close()
		r.inf = b.info()
		initErr <- nil
		for req := range r.jobs {
			res, err := execute(b, req.job)
			req.reply <- reply{res: res, err: err}
		}
	}()
	if err := <-initErr; err != nil {
		return nil, err
	}
	return r, nil
}

// Info reports the renderer of the context.
func (r *Renderer) Info() Info { return r.inf }

// Close stops the render goroutine and destroys the context.
func (r *Renderer) Close() { close(r.jobs) }

// Render queues a job and waits for its result. If ctx ends while the job is
// running, the job still completes on the render goroutine (GL work cannot be
// interrupted) but Render returns immediately.
func (r *Renderer) Render(ctx context.Context, job Job) (Result, error) {
	req := request{job: job, reply: make(chan reply, 1)}
	select {
	case r.jobs <- req:
	case <-ctx.Done():
		return Result{}, fmt.Errorf("egl: queue: %w", ctx.Err())
	}
	select {
	case rep := <-req.reply:
		return rep.res, rep.err
	case <-ctx.Done():
		return Result{}, fmt.Errorf("egl: render: %w", ctx.Err())
	}
}

func execute(b backend, job Job) (Result, error) {
	if job.Frames <= 0 || job.Width <= 0 || job.Height <= 0 {
		return Result{}, fmt.Errorf("egl: invalid job %dx%d x%d", job.Width, job.Height, job.Frames)
	}
	if err := b.useProgram(job.Source); err != nil {
		return Result{}, err
	}
	defer b.releaseProgram()
	if err := b.setTarget(job.Width, job.Height); err != nil {
		return Result{}, err
	}

	type timed struct {
		loc  int32
		base float64
	}
	var times []timed
	unit := int32(0)
	for _, u := range job.Uniforms {
		loc := b.location(u.Name)
		if loc < 0 {
			continue // not declared or optimized out
		}
		switch {
		case u.Type == "sampler2D":
			b.bindCamera(loc, unit)
			unit++
		case u.Semantic == "resolution":
			v := []float32{float32(job.Width), float32(job.Height), 1, 0}
			b.setUniform(loc, u.Type, v[:components(u.Type)])
		case u.Semantic == "time" && u.Type == "float":
			base := 0.0
			if len(u.Value) > 0 {
				base = u.Value[0]
			}
			times = append(times, timed{loc: loc, base: base})
		default:
			b.setUniform(loc, u.Type, toFloat32(u.Value, components(u.Type)))
		}
	}

	res := Result{Animated: len(times) > 0}
	frameMS := make([]float64, 0, job.Frames)
	sample := make([]float32, sampleSize*sampleSize*4)
	full := make([]float32, job.Width*job.Height*4)
	hashes := make([]uint64, 0, job.Frames)
	seed := maphash.MakeSeed() // hashes are only compared within this job

	for f := 0; f < job.Frames; f++ {
		if !job.Deadline.IsZero() && time.Now().After(job.Deadline) {
			res.Truncated = true
			break
		}
		for _, t := range times {
			b.setUniform(t.loc, "float", []float32{float32(t.base + float64(f)/fps)})
		}
		start := time.Now()
		b.drawFrame()
		frameMS = append(frameMS, float64(time.Since(start).Microseconds())/1000)

		// The nearest-neighbour sample is cheap but can miss thin features,
		// so black and constant frames are decided on the full frame.
		b.readSample(sample)
		st := inspect(sample)
		res.NaNInf = res.NaNInf || st.nanInf
		haveFull := false
		if st.black {
			b.readFull(full)
			haveFull = true
			if inspect(full).black {
				res.BlackFrames++
			}
		}
		if res.Animated {
			if !haveFull {
				b.readFull(full)
				haveFull = true
			}
			hashes = append(hashes, maphash.Bytes(seed, floatBytes(full)))
			if f >= constantLag && hashes[f-constantLag] == hashes[f] {
				res.ConstantFrames++
			}
		}
	}

	res.FramesRendered = len(frameMS)
	if res.FramesRendered == 0 {
		return res, nil
	}

	// Full-resolution NaN/Inf check on the last frame: the per-frame sample
	// can miss isolated pixels.
	if !res.NaNInf {
		b.readFull(full)
		res.NaNInf = inspect(full).nanInf
	}

	res.FirstFrameMS = frameMS[0]
	res.MedianMS, res.P95MS = frameStats(frameMS)
	return res, nil
}

// frameStats returns median and p95 frame time, discarding warm-up frames
// (shader JIT, caches): the first 10 when more than 20 frames were rendered,
// otherwise only the first one.
func frameStats(ms []float64) (median, p95 float64) {
	warm := 1
	if len(ms) > 20 {
		warm = 10
	}
	if len(ms) <= warm {
		warm = 0
	}
	s := append([]float64(nil), ms[warm:]...)
	sort.Float64s(s)
	return round3(percentile(s, 0.5)), round3(percentile(s, 0.95))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(idx-float64(lo))
}

type frameStat struct {
	nanInf bool
	black  bool
}

func inspect(px []float32) frameStat {
	st := frameStat{black: true}
	for i, v := range px {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			st.nanInf = true
			continue
		}
		if i%4 != 3 && f > blackEpsilon {
			st.black = false
		}
	}
	return st
}

func floatBytes(px []float32) []byte {
	if len(px) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&px[0])), len(px)*4)
}

// cameraImage is the deterministic RGBA8 test image bound to camera samplers:
// smooth RGB gradients modulated by a 32 px checkerboard, so filters that
// sample the camera see structure, edges and every luminance range.
func cameraImage() []byte {
	img := make([]byte, cameraSize*cameraSize*4)
	for y := 0; y < cameraSize; y++ {
		for x := 0; x < cameraSize; x++ {
			i := (y*cameraSize + x) * 4
			check := byte(0)
			if (x/32+y/32)%2 == 0 {
				check = 48
			}
			img[i] = byte(x*207/cameraSize) + check
			img[i+1] = byte(y*207/cameraSize) + check
			img[i+2] = byte((x+y)*103/cameraSize) + check
			img[i+3] = 255
		}
	}
	return img
}

func components(typ string) int {
	switch strings.TrimSpace(typ) {
	case "vec2", "ivec2":
		return 2
	case "vec3", "ivec3":
		return 3
	case "vec4", "ivec4":
		return 4
	default:
		return 1
	}
}

func toFloat32(v []float64, n int) []float32 {
	out := make([]float32, n)
	for i := 0; i < n && i < len(v); i++ {
		out[i] = float32(v[i])
	}
	return out
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
