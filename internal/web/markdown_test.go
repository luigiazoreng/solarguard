package web_test

import (
	"strings"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/web"
)

func TestMarkdownRendererSafe(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string
		refuses  []string
	}{
		{
			name:     "escapes raw HTML",
			input:    "Hello <script>alert(1)</script> <b>bold</b>",
			contains: []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;b&gt;bold&lt;/b&gt;"},
			refuses:  []string{"<script>", "<b>"},
		},
		{
			name:     "rejects javascript links",
			input:    "[malicious](javascript:alert(1))",
			contains: []string{"malicious"},
			refuses:  []string{"href=\"javascript:alert(1)\"", "javascript:"},
		},
		{
			name:     "allows safe http and https links",
			input:    "[Open-Meteo](https://open-meteo.com) and [local](/plants/1)",
			contains: []string{`href="https://open-meteo.com"`, `href="/plants/1"`},
		},
		{
			name:     "escapes raw HTML inside links",
			input:    `[<img src=x onerror=alert(1)>](https://example.com)`,
			contains: []string{"&lt;img src=x onerror=alert(1)&gt;"},
			refuses:  []string{"<img"},
		},
		{
			name:     "renders headings, lists, bold",
			input:    "# Relatório Mensal\n\n## Subtítulo\n\n- Ponto 1\n- Ponto 2 com **destaque**",
			contains: []string{"<h1>Relatório Mensal</h1>", "<h2>Subtítulo</h2>", "<ul>", "<li>Ponto 1</li>", "<strong>destaque</strong>", "</ul>"},
		},
		{
			name: "renders markdown table",
			input: `| Modelo | MAE |
|---|---|
| Ridge | 4,45 |`,
			contains: []string{"<table>", "<thead>", "<th>Modelo</th>", `<th class="num">MAE</th>`, "<tbody>", "<td>Ridge</td>", `<td class="num">4,45</td>`, "</table>"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(web.RenderMarkdown(tc.input))
			for _, c := range tc.contains {
				if !strings.Contains(got, c) {
					t.Errorf("expected output to contain %q, but got:\n%s", c, got)
				}
			}
			for _, r := range tc.refuses {
				if strings.Contains(got, r) {
					t.Errorf("output must NOT contain %q, but got:\n%s", r, got)
				}
			}
		})
	}
}

func TestRenderMarkdown_BlockquoteAndShortDigitLine(t *testing.T) {
	got := string(web.RenderMarkdown("> Estimativa **baixa** <b>x</b>\n\n2 k\n\n12. item"))
	for _, want := range []string{
		"<blockquote>Estimativa <strong>baixa</strong> &lt;b&gt;x&lt;/b&gt;</blockquote>",
		"<p>2 k</p>",
		"<ol>\n<li>item</li>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}
