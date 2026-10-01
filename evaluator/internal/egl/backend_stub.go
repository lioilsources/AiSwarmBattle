//go:build !(linux && cgo)

package egl

func newBackend(Options) (backend, error) { return nil, ErrUnavailable }
