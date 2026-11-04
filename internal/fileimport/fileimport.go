// Package fileimport parses and imports solar generation spreadsheets into SolarGuard.
package fileimport

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// StampSemantics defines how reading timestamps map to local operational days.
type StampSemantics string

const (
	SemanticsPreviousDay StampSemantics = "previous_day"
	SemanticsSameDay     StampSemantics = "same_day"
)

// Options specifies the parameters for importing a spreadsheet.
type Options struct {
	File           string
	Source         string
	PlantID        string // External ID
	PlantName      string
	Timezone       string
	StampSemantics StampSemantics
	AreaM2         *float64
	Efficiency     *float64
}

// Summary reports the results of an import operation.
type Summary struct {
	Rows            int
	Inserted        int
	TextCells       int
	UnparsableCells int
	Devices         int
	FirstDate       string
	LastDate        string
}

const (
	colHeaderDevice   = "Torre"
	colHeaderDateTime = "UTCDateTime"
	colHeaderValue    = "ActualValue (KW)"
)

var dateTimeLayouts = []string{
	"2006-01-02 15:04:05.000",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	time.DateOnly,
}

// ImportXLSX imports readings from an Excel spreadsheet into the store.
func ImportXLSX(ctx context.Context, st *store.Store, opts Options) (Summary, error) {
	if opts.StampSemantics != SemanticsPreviousDay && opts.StampSemantics != SemanticsSameDay {
		return Summary{}, fmt.Errorf("invalid stamp semantics %q: must be %q or %q",
			opts.StampSemantics, SemanticsPreviousDay, SemanticsSameDay)
	}

	loc, err := time.LoadLocation(opts.Timezone)
	if err != nil {
		return Summary{}, fmt.Errorf("load timezone %q: %w", opts.Timezone, err)
	}

	f, err := excelize.OpenFile(opts.File)
	if err != nil {
		return Summary{}, fmt.Errorf("open spreadsheet %q: %w", opts.File, err)
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return Summary{}, fmt.Errorf("spreadsheet has no sheets")
	}
	sheet := sheets[0]

	rows, err := f.GetRows(sheet)
	if err != nil {
		return Summary{}, fmt.Errorf("read sheet %q: %w", sheet, err)
	}
	if len(rows) == 0 {
		return Summary{}, fmt.Errorf("sheet %q is empty", sheet)
	}

	headerRow := rows[0]
	deviceCol := -1
	dateCol := -1
	valueCol := -1

	for colIdx, h := range headerRow {
		switch strings.TrimSpace(h) {
		case colHeaderDevice:
			deviceCol = colIdx + 1
		case colHeaderDateTime:
			dateCol = colIdx + 1
		case colHeaderValue:
			valueCol = colIdx + 1
		}
	}

	if deviceCol == -1 {
		return Summary{}, fmt.Errorf("missing required column %q", colHeaderDevice)
	}
	if dateCol == -1 {
		return Summary{}, fmt.Errorf("missing required column %q", colHeaderDateTime)
	}
	if valueCol == -1 {
		return Summary{}, fmt.Errorf("missing required column %q", colHeaderValue)
	}

	// Ensure Source
	source, err := st.EnsureSource(ctx, domain.Source{
		Kind:    "file",
		Name:    opts.Source,
		Enabled: true,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("ensure source: %w", err)
	}

	// Upsert Plant
	plant, err := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   source.ID,
		ExternalID: opts.PlantID,
		Name:       opts.PlantName,
		Timezone:   opts.Timezone,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("upsert plant: %w", err)
	}

	deviceMap := make(map[string]domain.Device)
	var readings []domain.Reading

	var summary Summary
	var minDate, maxDate string

	for r := 2; r <= len(rows); r++ {
		devCell, err := excelize.CoordinatesToCellName(deviceCol, r)
		if err != nil {
			return Summary{}, err
		}
		dateCell, err := excelize.CoordinatesToCellName(dateCol, r)
		if err != nil {
			return Summary{}, err
		}
		valCell, err := excelize.CoordinatesToCellName(valueCol, r)
		if err != nil {
			return Summary{}, err
		}

		devName, err := f.GetCellValue(sheet, devCell)
		if err != nil {
			return Summary{}, fmt.Errorf("row %d: read device cell: %w", r, err)
		}
		devName = strings.TrimSpace(devName)
		if devName == "" {
			continue // skip empty rows
		}

		dateStr, err := f.GetCellValue(sheet, dateCell)
		if err != nil {
			return Summary{}, fmt.Errorf("row %d: read date cell: %w", r, err)
		}
		dateStr = strings.TrimSpace(dateStr)

		cellType, err := f.GetCellType(sheet, valCell)
		if err != nil {
			return Summary{}, fmt.Errorf("row %d: get cell type: %w", r, err)
		}
		rawVal, err := f.GetCellValue(sheet, valCell, excelize.Options{RawCellValue: true})
		if err != nil {
			return Summary{}, fmt.Errorf("row %d: get raw cell value: %w", r, err)
		}

		// Ensure device
		dev, ok := deviceMap[devName]
		if !ok {
			d, err := st.UpsertDevice(ctx, domain.Device{
				PlantID:     plant.ID,
				ExternalID:  devName,
				Name:        devName,
				Kind:        "inverter",
				PanelAreaM2: opts.AreaM2,
				Efficiency:  opts.Efficiency,
			})
			if err != nil {
				return Summary{}, fmt.Errorf("upsert device %q: %w", devName, err)
			}
			deviceMap[devName] = d
			dev = d
		}

		// Parse UTC date and period
		utcTime, err := parseDate(dateStr)
		if err != nil {
			return Summary{}, fmt.Errorf("row %d: parse date %q: %w", r, dateStr, err)
		}

		localT := utcTime.In(loc)
		localDate := time.Date(localT.Year(), localT.Month(), localT.Day(), 0, 0, 0, 0, loc)
		var targetDate time.Time
		if opts.StampSemantics == SemanticsPreviousDay {
			targetDate = localDate.AddDate(0, 0, -1)
		} else {
			targetDate = localDate
		}
		dateOnly := targetDate.Format(time.DateOnly)

		periodStart, periodEnd, err := domain.LocalDay(dateOnly, opts.Timezone)
		if err != nil {
			return Summary{}, fmt.Errorf("row %d: local day bounds for %q: %w", r, dateOnly, err)
		}

		if minDate == "" || dateOnly < minDate {
			minDate = dateOnly
		}
		if maxDate == "" || dateOnly > maxDate {
			maxDate = dateOnly
		}

		// Parse value according to cell type
		isText := cellType == excelize.CellTypeSharedString || cellType == excelize.CellTypeInlineString
		var valPtr *float64

		if isText {
			summary.TextCells++
			cleaned := strings.TrimSpace(strings.ReplaceAll(rawVal, ",", "."))
			if cleaned != "" {
				parsed, err := strconv.ParseFloat(cleaned, 64)
				if err != nil {
					summary.UnparsableCells++
				} else {
					valPtr = &parsed // text cell is already in kWh
				}
			} else {
				summary.UnparsableCells++
			}
		} else {
			cleaned := strings.TrimSpace(strings.ReplaceAll(rawVal, ",", "."))
			if cleaned != "" {
				parsed, err := strconv.ParseFloat(cleaned, 64)
				if err != nil {
					summary.UnparsableCells++
				} else {
					kwh := parsed / 1000.0 // numeric cell is in Wh
					valPtr = &kwh
				}
			} else {
				summary.UnparsableCells++
			}
		}

		readings = append(readings, domain.Reading{
			DeviceID:      dev.ID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   periodStart,
			PeriodEnd:     periodEnd,
			Value:         valPtr,
			RawValue:      rawVal,
			Quality:       domain.QualityUnchecked,
			QualityReason: "",
		})
		summary.Rows++
	}

	inserted, err := st.InsertReadings(ctx, readings)
	if err != nil {
		return Summary{}, fmt.Errorf("insert readings: %w", err)
	}

	summary.Inserted = inserted
	summary.Devices = len(deviceMap)
	summary.FirstDate = minDate
	summary.LastDate = maxDate

	return summary, nil
}

func parseDate(s string) (time.Time, error) {
	for _, layout := range dateTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported date format: %q", s)
}
