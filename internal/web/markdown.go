package web

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
)

var (
	linkRegex = regexp.MustCompile(`\[(.*?)\]\((.*?)\)`)
	boldRegex = regexp.MustCompile(`\*\*(.*?)\*\*`)
	iterRegex = regexp.MustCompile(`\*(.*?)\*`)
)

func renderInlines(text string) string {
	// Process links: [text](url)
	text = linkRegex.ReplaceAllStringFunc(text, func(m string) string {
		sub := linkRegex.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		linkText := sub[1]
		linkURL := strings.TrimSpace(sub[2])

		// Reject unsafe schemes
		lowerURL := strings.ToLower(linkURL)
		if strings.HasPrefix(lowerURL, "javascript:") || strings.HasPrefix(lowerURL, "data:") || strings.HasPrefix(lowerURL, "vbscript:") {
			return linkText
		}

		parsed, err := url.Parse(linkURL)
		if err != nil {
			return linkText
		}
		if parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https" {
			return linkText
		}

		safeURL := html.EscapeString(linkURL)
		return `<a href="` + safeURL + `" rel="noopener noreferrer">` + linkText + `</a>`
	})

	// Bold: **text**
	text = boldRegex.ReplaceAllString(text, `<strong>$1</strong>`)
	// Italic: *text*
	text = iterRegex.ReplaceAllString(text, `<em>$1</em>`)

	return text
}

func isTableSeparator(line string) bool {
	trimmed := strings.Trim(line, " |")
	if len(trimmed) == 0 {
		return false
	}
	for _, c := range trimmed {
		if c != '-' && c != ':' && c != '|' && c != ' ' {
			return false
		}
	}
	return strings.Contains(trimmed, "-")
}

func splitTableCells(line string) []string {
	parts := strings.Split(line, "|")
	if len(parts) >= 2 {
		if strings.TrimSpace(parts[0]) == "" {
			parts = parts[1:]
		}
		if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
			parts = parts[:len(parts)-1]
		}
	}
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// isOrderedItem reports whether a line starts like "1. " or "12. ".
func isOrderedItem(line string) bool {
	dot := strings.Index(line, ". ")
	return dot > 0 && dot <= 3 && line[0] >= '0' && line[0] <= '9'
}

// RenderMarkdown safely converts a markdown string into template.HTML.
// Raw HTML is escaped first and never passed through. Links with unsafe schemes are rejected.
func RenderMarkdown(input string) template.HTML {
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	var out strings.Builder

	i := 0
	n := len(lines)

	for i < n {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			i++
			continue
		}

		// Table detection
		if strings.Contains(trimmed, "|") && i+1 < n && isTableSeparator(lines[i+1]) {
			headers := splitTableCells(trimmed)
			var rows [][]string

			i += 2 // skip header and separator
			for i < n && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
				rows = append(rows, splitTableCells(lines[i]))
				i++
			}
			out.WriteString(renderTable(headers, rows))
			continue
		}

		// Headings
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' && level < 6 {
				level++
			}
			headingText := strings.TrimSpace(trimmed[level:])
			escapedHeading := html.EscapeString(headingText)
			hTag := fmt.Sprintf("h%d", level)
			out.WriteString("<" + hTag + ">" + renderInlines(escapedHeading) + "</" + hTag + ">\n")
			i++
			continue
		}

		// Unordered list
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			out.WriteString("<ul>\n")
			for i < n {
				cur := strings.TrimSpace(lines[i])
				if strings.HasPrefix(cur, "- ") || strings.HasPrefix(cur, "* ") {
					itemText := strings.TrimSpace(cur[2:])
					escapedItem := html.EscapeString(itemText)
					out.WriteString("<li>" + renderInlines(escapedItem) + "</li>\n")
					i++
				} else if cur == "" {
					break
				} else {
					break
				}
			}
			out.WriteString("</ul>\n")
			continue
		}

		// Ordered list
		if isOrderedItem(trimmed) {
			out.WriteString("<ol>\n")
			for i < n {
				cur := strings.TrimSpace(lines[i])
				dotIdx := strings.Index(cur, ". ")
				if dotIdx > 0 && dotIdx <= 3 {
					itemText := strings.TrimSpace(cur[dotIdx+2:])
					escapedItem := html.EscapeString(itemText)
					out.WriteString("<li>" + renderInlines(escapedItem) + "</li>\n")
					i++
				} else if cur == "" {
					break
				} else {
					break
				}
			}
			out.WriteString("</ol>\n")
			continue
		}

		// Blockquote
		if strings.HasPrefix(trimmed, ">") {
			var quote []string
			for i < n {
				cur := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(cur, ">") {
					break
				}
				quote = append(quote, html.EscapeString(strings.TrimSpace(cur[1:])))
				i++
			}
			out.WriteString("<blockquote>" + renderInlines(strings.Join(quote, " ")) + "</blockquote>\n")
			continue
		}

		// Paragraph
		var pLines []string
		for i < n {
			cur := strings.TrimSpace(lines[i])
			if cur == "" || strings.HasPrefix(cur, "#") || strings.HasPrefix(cur, ">") || strings.HasPrefix(cur, "- ") ||
				strings.HasPrefix(cur, "* ") || (strings.Contains(cur, "|") && i+1 < n && isTableSeparator(lines[i+1])) {
				break
			}
			pLines = append(pLines, html.EscapeString(cur))
			i++
		}
		if len(pLines) > 0 {
			pText := renderInlines(strings.Join(pLines, " "))
			out.WriteString("<p>" + pText + "</p>\n")
		}
	}

	return template.HTML(out.String())
}

var numericCellRegex = regexp.MustCompile(`^[+\-−]?\d[\d.]*(,\d+)?(\s*(kWh|MWh|Wh|kW|W|%))?$`)

// isPlaceholderCell reports whether a cell stands for "no value".
func isPlaceholderCell(c string) bool {
	switch c {
	case "", "-", "—", "–", "N/A":
		return true
	}
	return false
}

// numericColumns marks the columns whose every non-placeholder cell is a
// pt-BR number, optionally with a unit. Those columns are right-aligned.
func numericColumns(headers []string, rows [][]string) []bool {
	cols := make([]bool, len(headers))
	for c := range headers {
		seen := false
		ok := true
		for _, row := range rows {
			if c >= len(row) {
				continue
			}
			cell := row[c]
			if isPlaceholderCell(cell) {
				continue
			}
			if !numericCellRegex.MatchString(cell) {
				ok = false
				break
			}
			seen = true
		}
		cols[c] = ok && seen
	}
	return cols
}

// badgeClass maps the severity and status labels written by the report
// generator to the badge classes in style.css. Only used in columns whose
// header is "Severidade" or "Status".
func badgeClass(header, cell string) string {
	switch strings.ToLower(header) {
	case "severidade":
		switch cell {
		case "crítico":
			return "badge-critical"
		case "aviso":
			return "badge-warning"
		case "informativo":
			return "badge-info"
		}
	case "status":
		switch cell {
		case "aberto":
			return "badge-warning"
		case "reconhecido":
			return "badge-info"
		case "resolvido":
			return "badge-success"
		}
	}
	return ""
}

// renderTable writes a table wrapped in the same container the other screens
// use. Cells are escaped before any inline formatting. Columns of numbers are
// right-aligned, severity and status labels become badges and long text cells
// are allowed to wrap.
func renderTable(headers []string, rows [][]string) string {
	numeric := numericColumns(headers, rows)
	var b strings.Builder
	b.WriteString("<div class=\"table-container\">\n<table>\n<thead>\n<tr>")
	for c, h := range headers {
		cls := ""
		if numeric[c] {
			cls = ` class="num"`
		}
		b.WriteString("<th" + cls + ">" + renderInlines(html.EscapeString(h)) + "</th>")
	}
	b.WriteString("</tr>\n</thead>\n<tbody>\n")
	for _, row := range rows {
		rowClass := ""
		for c, cell := range row {
			if c < len(headers) && strings.EqualFold(headers[c], "severidade") && cell == "crítico" {
				rowClass = ` class="row-critical"`
			}
		}
		b.WriteString("<tr" + rowClass + ">")
		for c, cell := range row {
			header := ""
			if c < len(headers) {
				header = headers[c]
			}
			escaped := renderInlines(html.EscapeString(cell))
			if bc := badgeClass(header, cell); bc != "" {
				b.WriteString("<td><span class=\"badge " + bc + "\">" + escaped + "</span></td>")
				continue
			}
			cls := ""
			switch {
			case c < len(numeric) && numeric[c]:
				cls = ` class="num"`
			case len([]rune(cell)) > 40:
				cls = ` class="cell-text"`
			}
			b.WriteString("<td" + cls + ">" + escaped + "</td>")
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</tbody>\n</table>\n</div>\n")
	return b.String()
}
