package chat

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// tableRuleRe is the line under a Markdown table's header: |---|:--:|
var tableRuleRe = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)

// tablesAsBlocks rewrites each Markdown table outside a code block as a fenced
// block with aligned columns, for a platform that has no tables. Inside the
// block nothing is formatted, so the markers in a cell are dropped.
func tablesAsBlocks(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	fenced := false
	for i := 0; i < len(lines); i++ {
		if strings.Count(lines[i], "```")%2 == 1 {
			fenced = !fenced
		}
		if fenced || !startsTable(lines, i) {
			out = append(out, lines[i])
			continue
		}
		rows := [][]string{tableCells(lines[i])}
		for i += 2; i < len(lines) && strings.Contains(lines[i], "|"); i++ {
			rows = append(rows, tableCells(lines[i]))
		}
		i--
		out = append(out, "```")
		out = append(out, alignRows(rows)...)
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

func startsTable(lines []string, i int) bool {
	return i+1 < len(lines) && strings.Contains(lines[i], "|") &&
		strings.Contains(lines[i+1], "|") && tableRuleRe.MatchString(lines[i+1])
}

// tableCells splits a row at its unescaped pipes and makes each cell plain text.
func tableCells(line string) []string {
	const escaped = "\x01"
	line = strings.ReplaceAll(strings.TrimSpace(line), `\|`, escaped)
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	cells := strings.Split(line, "|")
	plain := strings.NewReplacer("**", "", "__", "", "~~", "", "`", "", escaped, "|")
	for i, c := range cells {
		cells[i] = strings.TrimSpace(plain.Replace(linkRe.ReplaceAllString(c, "$1 ($2)")))
	}
	return cells
}

// alignRows pads the columns to one width each and rules off the first row.
func alignRows(rows [][]string) []string {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	join := func(cells []string) string {
		padded := make([]string, len(cells))
		for i, c := range cells {
			padded[i] = c + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c))
		}
		return strings.TrimRight(strings.Join(padded, " | "), " ")
	}
	rule := make([]string, len(widths))
	for i, w := range widths {
		rule[i] = strings.Repeat("-", w)
	}
	out := make([]string, 0, len(rows)+1)
	out = append(out, join(rows[0]), strings.Join(rule, "-+-"))
	for _, r := range rows[1:] {
		out = append(out, join(r))
	}
	return out
}
