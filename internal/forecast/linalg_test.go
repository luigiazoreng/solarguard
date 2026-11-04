package forecast_test

import (
	"math"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/forecast"
)

func TestSolveLinearSystem(t *testing.T) {
	// 2x2 system:
	// 2x + y = 5
	// x + 3y = 5
	// Solution: x = 2, y = 1
	A := [][]float64{
		{2, 1},
		{1, 3},
	}
	b := []float64{5, 5}
	x, err := forecast.SolveLinearSystem(A, b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(x[0]-2.0) > 1e-9 || math.Abs(x[1]-1.0) > 1e-9 {
		t.Errorf("expected [2, 1], got %v", x)
	}
}

func TestSolveLinearSystemSingular(t *testing.T) {
	// Degenerate/singular system: constant column / collinear
	A := [][]float64{
		{1, 2},
		{2, 4},
	}
	b := []float64{3, 6}
	// Must not crash!
	x, err := forecast.SolveLinearSystem(A, b)
	if err != nil {
		// Either returns error or robust fallback solution, but must never panic/crash
		t.Logf("singular system returned error gracefully: %v", err)
	} else {
		t.Logf("singular system returned robust solution: %v", x)
	}
}
