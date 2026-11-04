package forecast

import (
	"errors"
	"math"
)

// SolveLinearSystem solves A * x = b using Gaussian elimination with partial pivoting.
// If the matrix is singular or near-singular, it regularizes the diagonal slightly
// to provide a robust solution without crashing.
func SolveLinearSystem(A [][]float64, b []float64) ([]float64, error) {
	n := len(A)
	if n == 0 {
		return nil, errors.New("empty matrix")
	}
	if len(b) != n {
		return nil, errors.New("dimension mismatch")
	}

	// Make deep copy of A and b
	aCopy := make([][]float64, n)
	for i := range A {
		if len(A[i]) != n {
			return nil, errors.New("non-square matrix")
		}
		aCopy[i] = make([]float64, n)
		copy(aCopy[i], A[i])
	}
	bCopy := make([]float64, n)
	copy(bCopy, b)

	const eps = 1e-12
	const reg = 1e-6

	// Forward elimination with partial pivoting
	for k := 0; k < n; k++ {
		// Find pivot
		maxVal := math.Abs(aCopy[k][k])
		p := k
		for i := k + 1; i < n; i++ {
			if v := math.Abs(aCopy[i][k]); v > maxVal {
				maxVal = v
				p = i
			}
		}

		// Swap rows
		if p != k {
			aCopy[k], aCopy[p] = aCopy[p], aCopy[k]
			bCopy[k], bCopy[p] = bCopy[p], bCopy[k]
		}

		// If pivot is still near zero, regularize diagonal element to prevent division by zero
		if math.Abs(aCopy[k][k]) < eps {
			aCopy[k][k] += reg
		}

		// Eliminate below
		for i := k + 1; i < n; i++ {
			factor := aCopy[i][k] / aCopy[k][k]
			aCopy[i][k] = 0.0
			for j := k + 1; j < n; j++ {
				aCopy[i][j] -= factor * aCopy[k][j]
			}
			bCopy[i] -= factor * bCopy[k]
		}
	}

	// Back substitution
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		sum := bCopy[i]
		for j := i + 1; j < n; j++ {
			sum -= aCopy[i][j] * x[j]
		}
		if math.Abs(aCopy[i][i]) < eps {
			x[i] = 0.0
		} else {
			x[i] = sum / aCopy[i][i]
		}
		if math.IsNaN(x[i]) || math.IsInf(x[i], 0) {
			x[i] = 0.0
		}
	}

	return x, nil
}
