package web

import (
	"fmt"
	"strings"
	"time"
)

// FormatFloat formats a float using pt-BR notation (e.g. 1.234,56).
func FormatFloat(v float64, decimals int) string {
	s := fmt.Sprintf("%.*f", decimals, v)
	parts := strings.Split(s, ".")
	intPart := parts[0]
	decPart := ""
	if len(parts) > 1 {
		decPart = parts[1]
	}

	isNegative := false
	if strings.HasPrefix(intPart, "-") {
		isNegative = true
		intPart = intPart[1:]
	}

	var b strings.Builder
	n := len(intPart)
	for i, c := range intPart {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}

	res := b.String()
	if isNegative {
		res = "-" + res
	}
	if decimals > 0 && decPart != "" {
		res += "," + decPart
	}
	return res
}

// FormatInt formats an integer using pt-BR thousands separators (e.g. 1.234).
func FormatInt(n int64) string {
	return FormatFloat(float64(n), 0)
}

// FormatPercent formats a percentage value (e.g. 12.3% -> "12,3%").
func FormatPercent(pct float64) string {
	return FormatFloat(pct, 1) + "%"
}

// FormatDate converts YYYY-MM-DD into DD/MM/AAAA.
func FormatDate(d string) string {
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		return d
	}
	return t.Format("02/01/2006")
}

// FormatTimeInZone formats a timestamp in the specified time zone as DD/MM/AAAA HH:MM.
func FormatTimeInZone(t *time.Time, tz string) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("02/01/2006 15:04")
}
