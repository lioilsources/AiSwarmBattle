package egl

import (
	"math"
	"testing"
)

// scriptBackend replays per-frame pixel values; it exercises the measurement
// logic in execute without a GL context.
type scriptBackend struct {
	frame    int
	pixel    func(frame int) float32
	uniforms map[int32][]float32
	locs     map[string]int32
}

func (s *scriptBackend) info() Info               { return Info{} }
func (s *scriptBackend) useProgram(string) error  { return nil }
func (s *scriptBackend) releaseProgram()          {}
func (s *scriptBackend) setTarget(int, int) error { return nil }
func (s *scriptBackend) bindCamera(int32, int32)  {}
func (s *scriptBackend) close()                   {}
func (s *scriptBackend) drawFrame()               { s.frame++ }
func (s *scriptBackend) location(name string) int32 {
	if l, ok := s.locs[name]; ok {
		return l
	}
	return -1
}
func (s *scriptBackend) setUniform(loc int32, _ string, v []float32) {
	s.uniforms[loc] = append([]float32(nil), v...)
}
func (s *scriptBackend) readSample(dst []float32) { s.fill(dst) }
func (s *scriptBackend) readFull(dst []float32)   { s.fill(dst) }
func (s *scriptBackend) fill(dst []float32) {
	v := s.pixel(s.frame - 1)
	for i := range dst {
		dst[i] = v
	}
}

func TestExecute(t *testing.T) {
	timeU := Uniform{Name: "uTime", Type: "float", Semantic: "time", Value: []float64{0}}
	resU := Uniform{Name: "uResolution", Type: "vec2", Semantic: "resolution"}

	tests := []struct {
		name         string
		pixel        func(int) float32
		uniforms     []Uniform
		wantNaN      bool
		wantBlack    int
		wantConstant int
		wantAnimated bool
	}{
		{
			name:     "static shader, constant image is fine",
			pixel:    func(int) float32 { return 0.5 },
			uniforms: []Uniform{resU},
		},
		{
			name:         "animated but frozen output counts constant frames",
			pixel:        func(int) float32 { return 0.5 },
			uniforms:     []Uniform{resU, timeU},
			wantConstant: 60 - constantLag,
			wantAnimated: true,
		},
		{
			name:         "animated and changing",
			pixel:        func(f int) float32 { return float32(f) / 100 },
			uniforms:     []Uniform{timeU},
			wantBlack:    1, // frame 0 is 0.0
			wantAnimated: true,
		},
		{
			name:      "nan and black",
			pixel:     func(f int) float32 { return map[bool]float32{true: float32(math.NaN()), false: 0}[f == 5] },
			wantNaN:   true,
			wantBlack: 60,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &scriptBackend{
				pixel:    tc.pixel,
				uniforms: map[int32][]float32{},
				locs:     map[string]int32{"uTime": 1, "uResolution": 2},
			}
			res, err := execute(b, Job{Width: 8, Height: 16, Frames: 60, Uniforms: tc.uniforms})
			if err != nil {
				t.Fatal(err)
			}
			if res.FramesRendered != 60 {
				t.Errorf("frames = %d", res.FramesRendered)
			}
			if res.NaNInf != tc.wantNaN || res.BlackFrames != tc.wantBlack ||
				res.ConstantFrames != tc.wantConstant || res.Animated != tc.wantAnimated {
				t.Errorf("got nan=%v black=%d constant=%d animated=%v",
					res.NaNInf, res.BlackFrames, res.ConstantFrames, res.Animated)
			}
			if got, ok := b.uniforms[2]; ok && (len(got) != 2 || got[0] != 8 || got[1] != 16) {
				t.Errorf("resolution uniform = %v", got)
			}
			if tc.wantAnimated {
				if got := b.uniforms[1]; len(got) != 1 || math.Abs(float64(got[0])-59.0/60) > 1e-6 {
					t.Errorf("last uTime = %v", got)
				}
			}
		})
	}
}

func TestFrameStats(t *testing.T) {
	ms := make([]float64, 30)
	for i := range ms {
		ms[i] = 2
	}
	ms[0], ms[1] = 50, 40 // warm-up outliers are discarded
	ms[29] = 4
	med, p95 := frameStats(ms)
	if med != 2 {
		t.Errorf("median = %v", med)
	}
	if p95 <= 2 || p95 > 4 {
		t.Errorf("p95 = %v", p95)
	}
}

func TestCameraImageDeterministic(t *testing.T) {
	a, b := cameraImage(), cameraImage()
	if string(a) != string(b) || len(a) != cameraSize*cameraSize*4 {
		t.Fatal("camera image not deterministic")
	}
}
