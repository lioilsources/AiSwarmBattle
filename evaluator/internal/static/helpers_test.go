package static

import (
	"testing"

	"github.com/lioilsources/shaderbattle/pkg/types"
)

type analyzeResult struct{ types.StaticResult }

func (analyzeResult) eq(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", name, got, want)
	}
}

func (analyzeResult) eqb(t *testing.T, name string, got, want bool) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
