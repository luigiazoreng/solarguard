// Package reports builds deterministic reports from stored data, with no LLM.
package reports

import (
	"context"
	"errors"

	"github.com/luigiazoreng/solarguard/internal/store"
)

// ErrNotImplemented is returned until report generation is implemented.
var ErrNotImplemented = errors.New("geração de relatórios ainda não implementada")

// Request selects what to report on. From and To are local dates, inclusive.
type Request struct {
	PlantID int64
	Kind    string // weekly, monthly or period
	From    string
	To      string
}

// Generate builds a report and stores it with author system.
func Generate(ctx context.Context, st *store.Store, req Request) (store.Report, error) {
	return store.Report{}, ErrNotImplemented
}
