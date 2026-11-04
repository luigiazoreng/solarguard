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
			out.WriteString("<table>\n<thead>\n<tr>")
			headers := splitTableCells(trimmed)
			for _, h := range headers {
				escapedHeader := html.EscapeString(h)
				out.WriteString("<th>" + renderInlines(escapedHeader) + "</th>")
			}
			out.WriteString("</tr>\n</thead>\n<tbody>\n")

			i += 2 // skip header and separator
			for i < n && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
				rowCells := splitTableCells(lines[i])
				out.WriteString("<tr>")
				for _, c := range rowCells {
					escapedCell := html.EscapeString(c)
					out.WriteString("<td>" + renderInlines(escapedCell) + "</td>")
				}
				out.WriteString("</tr>\n")
				i++
			}
			out.WriteString("</tbody>\n</table>\n")
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
		if len(trimmed) > 2 && trimmed[0] >= '0' && trimmed[0] <= '9' && (strings.Contains(trimmed[:4], ". ")) {
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

		// Paragraph
		var pLines []string
		for i < n {
			cur := strings.TrimSpace(lines[i])
			if cur == "" || strings.HasPrefix(cur, "#") || strings.HasPrefix(cur, "- ") ||
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
