package main

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	tableast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var tableParser = goldmark.New(goldmark.WithExtensions(extension.Table)).Parser()

// Telegram's fixed-width table rendering wraps long cells mid-row. Keep
// each row together as normal text, with inline Markdown still available.
func wrapTelegramTables(markdown string) string {
	if !strings.Contains(markdown, "|") {
		return markdown
	}
	source := []byte(markdown)
	doc := tableParser.Parse(text.NewReader(source))
	var out strings.Builder
	offset := 0
	for node := doc.FirstChild(); node != nil; node = node.NextSibling() {
		table, ok := node.(*tableast.Table)
		if !ok || table.ChildCount() < 2 {
			continue
		}
		header := table.FirstChild()
		start := strings.LastIndexByte(markdown[:header.Pos()], '\n') + 1
		last := table.LastChild().Pos()
		end := len(markdown)
		if newline := strings.IndexByte(markdown[last:], '\n'); newline >= 0 {
			end = last + newline + 1
		}
		out.WriteString(markdown[offset:start])
		out.WriteString("\n")
		headers := telegramTableCells(source, header)
		for row := header.NextSibling(); row != nil; row = row.NextSibling() {
			cells := telegramTableCells(source, row)
			title := cells[0]
			if title != "" {
				if !strings.Contains(title, "**") {
					title = "**" + title + "**"
				}
				out.WriteString(title + "\n\n")
			}
			for i, cell := range cells[1:] {
				if cell == "" {
					continue
				}
				if len(cells) > 2 {
					out.WriteString("- " + headers[i+1] + ": ")
				}
				out.WriteString(cell + "\n")
			}
			out.WriteString("\n")
		}
		offset = end
	}
	if offset == 0 {
		return markdown
	}
	out.WriteString(markdown[offset:])
	return out.String()
}

func telegramTableCells(source []byte, row ast.Node) []string {
	cells := make([]string, 0, row.ChildCount())
	for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
		value := ""
		if cell.Lines().Len() > 0 {
			segment := cell.Lines().At(0)
			value = strings.TrimSpace(string(segment.Value(source)))
		}
		cells = append(cells, value)
	}
	return cells
}
