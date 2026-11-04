// Package inmet parses annual automatic station weather CSV files from INMET.
package inmet

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/encoding/charmap"

	"github.com/luigiazoreng/solarguard/internal/connector"
)

// Station contains metadata for an INMET automatic weather station.
type Station struct {
	Region    string
	State     string
	Name      string
	Code      string
	Latitude  float64
	Longitude float64
	Altitude  float64
	Founded   string
}

type dayData struct {
	date           string
	radSumKJ       float64
	daytimePresent map[int]bool // local hours 7 to 17 present

	precipSum   float64
	precipCount int

	tempSum   float64
	tempCount int
	tempMax   *float64

	humSum   float64
	humCount int
}

// ParseFile parses an INMET automatic station CSV file into Station metadata
// and a slice of connector.WeatherDay grouped by local day in the given timezone.
func ParseFile(r io.Reader, timezone string) (Station, []connector.WeatherDay, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return Station{}, nil, fmt.Errorf("load timezone %q: %w", timezone, err)
	}

	latin1Reader := charmap.ISO8859_1.NewDecoder().Reader(r)
	cr := csv.NewReader(latin1Reader)
	cr.Comma = ';'
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true

	var station Station
	var colDate, colHour, colPrecip, colRad, colTemp, colHum = -1, -1, -1, -1, -1, -1
	headerFound := false

	daysMap := make(map[string]*dayData)

	for {
		record, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Station{}, nil, fmt.Errorf("read csv record: %w", err)
		}
		if len(record) == 0 {
			continue
		}

		// First, parse metadata lines until the column header row is found.
		if !headerFound {
			firstNorm := normalizeHeader(record[0])
			if firstNorm == "DATA" {
				// Column header row found!
				headerFound = true
				for i, col := range record {
					norm := normalizeHeader(col)
					switch {
					case norm == "DATA" && colDate == -1:
						colDate = i
					case (norm == "HORA (UTC)" || norm == "HORA UTC") && colHour == -1:
						colHour = i
					case strings.Contains(norm, "PRECIPITACAO TOTAL") && colPrecip == -1:
						colPrecip = i
					case strings.Contains(norm, "RADIACAO GLOBAL") && colRad == -1:
						colRad = i
					case strings.Contains(norm, "TEMPERATURA") && strings.Contains(norm, "BULBO SECO") && colTemp == -1:
						colTemp = i
					case strings.Contains(norm, "UMIDADE RELATIVA") && colHum == -1:
						colHum = i
					}
				}

				// Validate required columns
				if colDate == -1 {
					return Station{}, nil, errors.New("missing required column: DATA")
				}
				if colHour == -1 {
					return Station{}, nil, errors.New("missing required column: HORA (UTC)")
				}
				if colPrecip == -1 {
					return Station{}, nil, errors.New("missing required column: PRECIPITACAO TOTAL, HORARIO (mm)")
				}
				if colRad == -1 {
					return Station{}, nil, errors.New("missing required column: RADIACAO GLOBAL (Kj/m2)")
				}
				if colTemp == -1 {
					return Station{}, nil, errors.New("missing required column: TEMPERATURA DO AR - BULBO SECO, HORARIA (C)")
				}
				if colHum == -1 {
					return Station{}, nil, errors.New("missing required column: UMIDADE RELATIVA DO AR, HORARIA (%)")
				}
				continue
			}

			// Parse metadata row
			parseMetadataRow(&station, record)
			continue
		}

		// Data rows
		maxCol := colDate
		for _, idx := range []int{colHour, colPrecip, colRad, colTemp, colHum} {
			if idx > maxCol {
				maxCol = idx
			}
		}
		if len(record) <= maxCol {
			continue
		}

		dateField := strings.TrimSpace(record[colDate])
		hourField := strings.TrimSpace(record[colHour])
		if dateField == "" || hourField == "" {
			continue
		}

		utcTime, err := parseDateAndHour(dateField, hourField)
		if err != nil {
			continue
		}

		locTime := utcTime.In(loc)
		localDate := locTime.Format("2006-01-02")
		localHour := locTime.Hour()

		day, ok := daysMap[localDate]
		if !ok {
			day = &dayData{
				date:           localDate,
				daytimePresent: make(map[int]bool),
			}
			daysMap[localDate] = day
		}

		// Radiation
		radVal, err := parseNumber(record[colRad])
		if err == nil && radVal != nil {
			day.radSumKJ += *radVal
			if localHour >= 7 && localHour <= 17 {
				day.daytimePresent[localHour] = true
			}
		}

		// Precipitation
		precipVal, err := parseNumber(record[colPrecip])
		if err == nil && precipVal != nil {
			day.precipSum += *precipVal
			day.precipCount++
		}

		// Temperature
		tempVal, err := parseNumber(record[colTemp])
		if err == nil && tempVal != nil {
			day.tempSum += *tempVal
			day.tempCount++
			if day.tempMax == nil || *tempVal > *day.tempMax {
				v := *tempVal
				day.tempMax = &v
			}
		}

		// Humidity
		humVal, err := parseNumber(record[colHum])
		if err == nil && humVal != nil {
			day.humSum += *humVal
			day.humCount++
		}
	}

	if !headerFound {
		return Station{}, nil, errors.New("csv column header not found")
	}

	// Sort dates ascending
	dates := make([]string, 0, len(daysMap))
	for d := range daysMap {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	outDays := make([]connector.WeatherDay, len(dates))
	for i, d := range dates {
		day := daysMap[d]
		wd := connector.WeatherDay{Date: d}

		// Radiation: sum divided by 3600. Nil if more than 2 daytime hours (7..17) are missing.
		missingDaytime := 0
		for h := 7; h <= 17; h++ {
			if !day.daytimePresent[h] {
				missingDaytime++
			}
		}
		if missingDaytime <= 2 {
			kwh := day.radSumKJ / 3600.0
			wd.IrradiationKWhM2 = &kwh
		}

		// Precipitation
		if day.precipCount > 0 {
			p := day.precipSum
			wd.PrecipitationMM = &p
		}

		// Temperature
		if day.tempCount > 0 {
			mean := day.tempSum / float64(day.tempCount)
			wd.TemperatureMeanC = &mean
			wd.TemperatureMaxC = day.tempMax
		}

		// Humidity
		if day.humCount > 0 {
			mean := day.humSum / float64(day.humCount)
			wd.HumidityMeanPct = &mean
		}

		outDays[i] = wd
	}

	return station, outDays, nil
}

func parseMetadataRow(station *Station, record []string) {
	var key, val string
	if len(record) >= 2 && record[1] != "" {
		key = strings.TrimSpace(record[0])
		val = strings.TrimSpace(record[1])
	} else if len(record) >= 1 && strings.Contains(record[0], ":") {
		parts := strings.SplitN(record[0], ":", 2)
		key = strings.TrimSpace(parts[0])
		val = strings.TrimSpace(parts[1])
	} else {
		return
	}

	normKey := normalizeHeader(key)
	normKey = strings.TrimSuffix(normKey, ":")

	switch {
	case normKey == "REGIAO" || strings.HasPrefix(normKey, "REGIAO"):
		station.Region = val
	case normKey == "UF":
		station.State = val
	case normKey == "ESTACAO" || strings.HasPrefix(normKey, "ESTACAO"):
		station.Name = val
	case strings.HasPrefix(normKey, "CODIGO"):
		station.Code = val
	case normKey == "LATITUDE":
		if v, err := parseNumber(val); err == nil && v != nil {
			station.Latitude = *v
		}
	case normKey == "LONGITUDE":
		if v, err := parseNumber(val); err == nil && v != nil {
			station.Longitude = *v
		}
	case normKey == "ALTITUDE":
		if v, err := parseNumber(val); err == nil && v != nil {
			station.Altitude = *v
		}
	case strings.HasPrefix(normKey, "DATA DE FUNDACAO"):
		station.Founded = val
	}
}

func parseDateAndHour(dateStr, hourStr string) (time.Time, error) {
	dateStr = strings.ReplaceAll(dateStr, "/", "-")
	dateParts := strings.Split(dateStr, "-")
	if len(dateParts) != 3 {
		return time.Time{}, fmt.Errorf("invalid date format: %q", dateStr)
	}
	y, err := strconv.Atoi(dateParts[0])
	if err != nil {
		return time.Time{}, err
	}
	m, err := strconv.Atoi(dateParts[1])
	if err != nil {
		return time.Time{}, err
	}
	d, err := strconv.Atoi(dateParts[2])
	if err != nil {
		return time.Time{}, err
	}

	hStr := strings.TrimSpace(hourStr)
	hStr = strings.TrimSuffix(strings.ToUpper(hStr), " UTC")
	hStr = strings.TrimSpace(hStr)

	var hour, min int
	if strings.Contains(hStr, ":") {
		parts := strings.Split(hStr, ":")
		hour, err = strconv.Atoi(parts[0])
		if err != nil {
			return time.Time{}, err
		}
		if len(parts) > 1 {
			min, _ = strconv.Atoi(parts[1])
		}
	} else {
		if len(hStr) == 4 {
			hour, err = strconv.Atoi(hStr[:2])
			if err != nil {
				return time.Time{}, err
			}
			min, _ = strconv.Atoi(hStr[2:])
		} else {
			hour, err = strconv.Atoi(hStr)
			if err != nil {
				return time.Time{}, err
			}
		}
	}

	return time.Date(y, time.Month(m), d, hour, min, 0, 0, time.UTC), nil
}

func parseNumber(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-9999" || s == "-9999.0" || s == "-9999,0" {
		return nil, nil
	}
	s = strings.ReplaceAll(s, ",", ".")
	if strings.HasPrefix(s, ".") {
		s = "0" + s
	} else if strings.HasPrefix(s, "-.") {
		s = "-0." + s[2:]
	}
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	if val <= -9998 {
		return nil, nil
	}
	return &val, nil
}

// normalizeHeader converts a header string to uppercase and strips accents and special degree/superscript characters.
func normalizeHeader(s string) string {
	s = strings.ToUpper(s)
	s = strings.ReplaceAll(s, "²", "2")
	s = strings.ReplaceAll(s, "°", "")
	s = strings.ReplaceAll(s, "º", "")

	var sb strings.Builder
	for _, r := range s {
		switch r {
		case 'Á', 'À', 'Â', 'Ã', 'Ä':
			sb.WriteRune('A')
		case 'É', 'È', 'Ê', 'Ë':
			sb.WriteRune('E')
		case 'Í', 'Ì', 'Î', 'Ï':
			sb.WriteRune('I')
		case 'Ó', 'Ò', 'Ô', 'Õ', 'Ö':
			sb.WriteRune('O')
		case 'Ú', 'Ù', 'Û', 'Ü':
			sb.WriteRune('U')
		case 'Ç':
			sb.WriteRune('C')
		default:
			if unicode.IsGraphic(r) {
				sb.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(sb.String())
}
