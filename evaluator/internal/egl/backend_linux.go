//go:build linux && cgo

package egl

/*
#cgo LDFLAGS: -lEGL -lGLESv2

#include <stdlib.h>
#include <string.h>
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES3/gl3.h>

#ifndef EGL_DEVICE_EXT
#define EGL_DEVICE_EXT 0x322C
#endif

typedef struct {
	EGLDisplay dpy;
	EGLContext ctx;
	EGLSurface surf;
	GLuint vs, prog;
	GLuint fbo, color, sfbo, scolor, camera;
	int w, h;
} ev_state;

static ev_state S;

static const char *ev_vs_src =
	"#version 300 es\n"
	"void main() {\n"
	"  vec2 p = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));\n"
	"  gl_Position = vec4(p * 2.0 - 1.0, 0.0, 1.0);\n"
	"}\n";

static int ev_is_software_device(EGLDeviceEXT dev) {
	PFNEGLQUERYDEVICESTRINGEXTPROC qs =
		(PFNEGLQUERYDEVICESTRINGEXTPROC)eglGetProcAddress("eglQueryDeviceStringEXT");
	if (!qs) return 0;
	const char *ext = qs(dev, EGL_EXTENSIONS);
	return ext && strstr(ext, "EGL_MESA_device_software") != NULL;
}

// ev_open_display picks an EGL device: hardware first unless prefer_sw,
// falling back to the default display.
static EGLDisplay ev_open_display(int prefer_sw) {
	PFNEGLQUERYDEVICESEXTPROC qd =
		(PFNEGLQUERYDEVICESEXTPROC)eglGetProcAddress("eglQueryDevicesEXT");
	PFNEGLGETPLATFORMDISPLAYEXTPROC gpd =
		(PFNEGLGETPLATFORMDISPLAYEXTPROC)eglGetProcAddress("eglGetPlatformDisplayEXT");
	if (qd && gpd) {
		EGLDeviceEXT devs[16];
		EGLint n = 0;
		if (qd(16, devs, &n)) {
			for (int pass = 0; pass < 2; pass++) {
				for (EGLint i = 0; i < n; i++) {
					int sw = ev_is_software_device(devs[i]);
					int want_sw = (pass == 0) ? prefer_sw : !prefer_sw;
					if (sw != want_sw) continue;
					EGLDisplay d = gpd(EGL_PLATFORM_DEVICE_EXT, devs[i], NULL);
					if (d != EGL_NO_DISPLAY && eglInitialize(d, NULL, NULL)) return d;
				}
			}
		}
	}
	EGLDisplay d = eglGetDisplay(EGL_DEFAULT_DISPLAY);
	if (d != EGL_NO_DISPLAY && eglInitialize(d, NULL, NULL)) return d;
	return EGL_NO_DISPLAY;
}

static const char *ev_init(int prefer_sw) {
	memset(&S, 0, sizeof(S));
	S.dpy = ev_open_display(prefer_sw);
	if (S.dpy == EGL_NO_DISPLAY) return "no EGL display";
	if (!eglBindAPI(EGL_OPENGL_ES_API)) return "eglBindAPI(GLES) failed";

	EGLint pbuf_attrs[] = {
		EGL_SURFACE_TYPE, EGL_PBUFFER_BIT,
		EGL_RENDERABLE_TYPE, EGL_OPENGL_ES3_BIT,
		EGL_RED_SIZE, 8, EGL_GREEN_SIZE, 8, EGL_BLUE_SIZE, 8, EGL_ALPHA_SIZE, 8,
		EGL_NONE,
	};
	EGLint nosurf_attrs[] = {
		EGL_RENDERABLE_TYPE, EGL_OPENGL_ES3_BIT,
		EGL_NONE,
	};
	EGLConfig cfg;
	EGLint n = 0;
	int pbuffer = 1;
	if (!eglChooseConfig(S.dpy, pbuf_attrs, &cfg, 1, &n) || n == 0) {
		pbuffer = 0;
		if (!eglChooseConfig(S.dpy, nosurf_attrs, &cfg, 1, &n) || n == 0)
			return "no GLES 3.0 EGL config";
	}
	EGLint ctx_attrs[] = { EGL_CONTEXT_CLIENT_VERSION, 3, EGL_NONE };
	S.ctx = eglCreateContext(S.dpy, cfg, EGL_NO_CONTEXT, ctx_attrs);
	if (S.ctx == EGL_NO_CONTEXT) return "eglCreateContext(GLES 3.0) failed";

	S.surf = EGL_NO_SURFACE;
	if (pbuffer) {
		EGLint sattrs[] = { EGL_WIDTH, 16, EGL_HEIGHT, 16, EGL_NONE };
		S.surf = eglCreatePbufferSurface(S.dpy, cfg, sattrs);
	}
	if (!eglMakeCurrent(S.dpy, S.surf, S.surf, S.ctx)) return "eglMakeCurrent failed";

	const char *ext = (const char *)glGetString(GL_EXTENSIONS);
	if (!ext || !strstr(ext, "GL_EXT_color_buffer_float"))
		return "GL_EXT_color_buffer_float not supported (needed for NaN/Inf readback)";

	S.vs = glCreateShader(GL_VERTEX_SHADER);
	glShaderSource(S.vs, 1, &ev_vs_src, NULL);
	glCompileShader(S.vs);
	GLint ok = 0;
	glGetShaderiv(S.vs, GL_COMPILE_STATUS, &ok);
	if (!ok) return "fullscreen vertex shader failed to compile";

	glGenFramebuffers(1, &S.fbo);
	glGenTextures(1, &S.color);
	glGenFramebuffers(1, &S.sfbo);
	glGenTextures(1, &S.scolor);
	glBindTexture(GL_TEXTURE_2D, S.scolor);
	glTexStorage2D(GL_TEXTURE_2D, 1, GL_RGBA32F, 128, 128);
	glBindFramebuffer(GL_FRAMEBUFFER, S.sfbo);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, S.scolor, 0);
	if (glCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE)
		return "RGBA32F sample framebuffer incomplete";
	return NULL;
}

static void ev_upload_camera(const unsigned char *px, int size) {
	glGenTextures(1, &S.camera);
	glBindTexture(GL_TEXTURE_2D, S.camera);
	glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA8, size, size, 0, GL_RGBA, GL_UNSIGNED_BYTE, px);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_S, GL_CLAMP_TO_EDGE);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_T, GL_CLAMP_TO_EDGE);
}

// ev_program compiles and links src; on failure returns a malloc'd log.
static char *ev_program(const char *src) {
	GLuint fs = glCreateShader(GL_FRAGMENT_SHADER);
	glShaderSource(fs, 1, &src, NULL);
	glCompileShader(fs);
	GLint ok = 0;
	glGetShaderiv(fs, GL_COMPILE_STATUS, &ok);
	if (!ok) {
		char *log = calloc(4096, 1);
		glGetShaderInfoLog(fs, 4095, NULL, log);
		glDeleteShader(fs);
		return log;
	}
	GLuint p = glCreateProgram();
	glAttachShader(p, S.vs);
	glAttachShader(p, fs);
	glLinkProgram(p);
	glDeleteShader(fs);
	glGetProgramiv(p, GL_LINK_STATUS, &ok);
	if (!ok) {
		char *log = calloc(4096, 1);
		glGetProgramInfoLog(p, 4095, NULL, log);
		glDeleteProgram(p);
		return log;
	}
	S.prog = p;
	glUseProgram(p);
	return NULL;
}

static void ev_release_program(void) {
	glUseProgram(0);
	if (S.prog) glDeleteProgram(S.prog);
	S.prog = 0;
}

static const char *ev_target(int w, int h) {
	if (w == S.w && h == S.h) return NULL;
	glDeleteTextures(1, &S.color);
	glGenTextures(1, &S.color);
	glBindTexture(GL_TEXTURE_2D, S.color);
	glTexStorage2D(GL_TEXTURE_2D, 1, GL_RGBA32F, w, h);
	glBindFramebuffer(GL_FRAMEBUFFER, S.fbo);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, S.color, 0);
	if (glCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE)
		return "RGBA32F render target incomplete";
	S.w = w;
	S.h = h;
	return NULL;
}

static void ev_draw(void) {
	glBindFramebuffer(GL_FRAMEBUFFER, S.fbo);
	glViewport(0, 0, S.w, S.h);
	glDrawArrays(GL_TRIANGLES, 0, 3);
	glFinish();
}

static void ev_read_sample(float *dst) {
	glBindFramebuffer(GL_READ_FRAMEBUFFER, S.fbo);
	glBindFramebuffer(GL_DRAW_FRAMEBUFFER, S.sfbo);
	glBlitFramebuffer(0, 0, S.w, S.h, 0, 0, 128, 128, GL_COLOR_BUFFER_BIT, GL_NEAREST);
	glBindFramebuffer(GL_READ_FRAMEBUFFER, S.sfbo);
	glReadPixels(0, 0, 128, 128, GL_RGBA, GL_FLOAT, dst);
}

static void ev_read_full(float *dst) {
	glBindFramebuffer(GL_READ_FRAMEBUFFER, S.fbo);
	glReadPixels(0, 0, S.w, S.h, GL_RGBA, GL_FLOAT, dst);
}

static GLint ev_location(const char *name) { return glGetUniformLocation(S.prog, name); }

static void ev_bind_camera(GLint loc, GLint unit) {
	glActiveTexture(GL_TEXTURE0 + unit);
	glBindTexture(GL_TEXTURE_2D, S.camera);
	glUniform1i(loc, unit);
}

static void ev_close(void) {
	if (S.dpy == EGL_NO_DISPLAY) return;
	eglMakeCurrent(S.dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
	if (S.surf != EGL_NO_SURFACE) eglDestroySurface(S.dpy, S.surf);
	if (S.ctx != EGL_NO_CONTEXT) eglDestroyContext(S.dpy, S.ctx);
	eglTerminate(S.dpy);
	S.dpy = EGL_NO_DISPLAY;
}
*/
import "C"

import (
	"fmt"
	"strings"
	"unsafe"
)

type glBackend struct {
	inf Info
}

func newBackend(opts Options) (backend, error) {
	preferSW := C.int(0)
	if opts.PreferSoftware {
		preferSW = 1
	}
	if msg := C.ev_init(preferSW); msg != nil {
		C.ev_close()
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(msg))
	}
	img := cameraImage()
	C.ev_upload_camera((*C.uchar)(unsafe.Pointer(&img[0])), C.int(cameraSize))

	renderer := C.GoString((*C.char)(unsafe.Pointer(C.glGetString(C.GL_RENDERER))))
	version := C.GoString((*C.char)(unsafe.Pointer(C.glGetString(C.GL_VERSION))))
	lower := strings.ToLower(renderer)
	return &glBackend{inf: Info{
		Renderer: renderer,
		Version:  version,
		Software: strings.Contains(lower, "llvmpipe") || strings.Contains(lower, "softpipe") ||
			strings.Contains(lower, "swiftshader") || strings.Contains(lower, "software"),
	}}, nil
}

func (b *glBackend) info() Info { return b.inf }

func (b *glBackend) useProgram(src string) error {
	cs := C.CString(src)
	defer C.free(unsafe.Pointer(cs))
	if log := C.ev_program(cs); log != nil {
		defer C.free(unsafe.Pointer(log))
		return &CompileError{Log: strings.TrimSpace(C.GoString(log))}
	}
	return nil
}

func (b *glBackend) releaseProgram() { C.ev_release_program() }

func (b *glBackend) location(name string) int32 {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	return int32(C.ev_location(cs))
}

func (b *glBackend) setUniform(loc int32, typ string, v []float32) {
	l := C.GLint(loc)
	f := func(i int) C.GLfloat { return C.GLfloat(v[i]) }
	n := func(i int) C.GLint { return C.GLint(v[i]) }
	switch typ {
	case "float":
		C.glUniform1f(l, f(0))
	case "vec2":
		C.glUniform2f(l, f(0), f(1))
	case "vec3":
		C.glUniform3f(l, f(0), f(1), f(2))
	case "vec4":
		C.glUniform4f(l, f(0), f(1), f(2), f(3))
	case "int", "bool":
		C.glUniform1i(l, n(0))
	case "ivec2":
		C.glUniform2i(l, n(0), n(1))
	case "ivec3":
		C.glUniform3i(l, n(0), n(1), n(2))
	case "ivec4":
		C.glUniform4i(l, n(0), n(1), n(2), n(3))
	}
}

func (b *glBackend) bindCamera(loc int32, unit int32) {
	C.ev_bind_camera(C.GLint(loc), C.GLint(unit))
}

func (b *glBackend) setTarget(w, h int) error {
	if msg := C.ev_target(C.int(w), C.int(h)); msg != nil {
		return fmt.Errorf("egl: %s", C.GoString(msg))
	}
	return nil
}

func (b *glBackend) drawFrame() { C.ev_draw() }

func (b *glBackend) readSample(dst []float32) {
	C.ev_read_sample((*C.float)(unsafe.Pointer(&dst[0])))
}

func (b *glBackend) readFull(dst []float32) {
	C.ev_read_full((*C.float)(unsafe.Pointer(&dst[0])))
}

func (b *glBackend) close() { C.ev_close() }
