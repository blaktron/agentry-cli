package decode

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const maxTableRows = 10000
const maxTableColumns = 1024
const maxTableBytes = 10 << 20

func markdownTable(rows [][]string) string {
	columns := 0
	for _, row := range rows {
		columns = max(columns, min(len(row), maxTableColumns))
	}
	if columns == 0 {
		return ""
	}
	var b strings.Builder
	write := func(text string) bool {
		available := maxTableBytes + 1 - b.Len()
		if len(text) > available {
			b.WriteString(text[:max(0, available)])
			return false
		}
		b.WriteString(text)
		return true
	}
	writeRow := func(row []string) bool {
		if !write("|") {
			return false
		}
		for i := 0; i < columns; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
				if len(cell) > maxTableBytes+1 {
					cell = cell[:maxTableBytes+1]
				}
			}
			cell = strings.ReplaceAll(cell, "\\", "\\\\")
			cell = strings.ReplaceAll(cell, "|", "\\|")
			cell = strings.ReplaceAll(strings.ReplaceAll(cell, "\r\n", "\n"), "\n", "<br>")
			if !write(" " + cell + " |") {
				return false
			}
		}
		return write("\n")
	}
	if !writeRow(rows[0]) {
		return b.String()
	}
	if !writeRow(strings.Split(strings.TrimSuffix(strings.Repeat("---,", columns), ","), ",")) {
		return b.String()
	}
	for _, row := range rows[1:min(len(rows), maxTableRows)] {
		if !writeRow(row) {
			return b.String()
		}
	}
	if len(rows) > maxTableRows {
		b.WriteString("\nTable cut at 10,000 rows.\n")
	}
	return b.String()
}

// OpenDocument keeps the document in one bounded content.xml. Decode
// structural names, never embedded scripts, links or other package parts.
var odfContentRE = regexp.MustCompile(`^content\.xml$`)

type odfCell struct {
	Repeat     int            `xml:"number-columns-repeated,attr"`
	Value      string         `xml:"value,attr"`
	Paragraphs []odfParagraph `xml:"p"`
}
type odfRow struct {
	Repeat int       `xml:"number-rows-repeated,attr"`
	Cells  []odfCell `xml:"table-cell"`
}
type odfSheet struct {
	Name string   `xml:"name,attr"`
	Rows []odfRow `xml:"table-row"`
}

func (s *odfSheet) UnmarshalXML(dec *xml.Decoder, start xml.StartElement) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch v := tok.(type) {
		case xml.StartElement:
			// Header rows can sit under table-header-rows rather than
			// directly under the sheet. Each row is decoded once either way.
			if v.Name.Local == "table-row" {
				if len(s.Rows) > maxTableRows {
					if err := dec.Skip(); err != nil {
						return err
					}
					continue
				}
				var row odfRow
				if err := dec.DecodeElement(&row, &v); err != nil {
					return err
				}
				s.Rows = append(s.Rows, row)
			}
		case xml.EndElement:
			if v.Name == start.Name {
				return nil
			}
		}
	}
}

// Character data inside formatting spans belongs to the same paragraph.
type odfParagraph string

func (p *odfParagraph) UnmarshalXML(dec *xml.Decoder, start xml.StartElement) error {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch v := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			b.Write(v)
		}
	}
	*p = odfParagraph(b.String())
	return nil
}

func odfContent(src string, budget int64) ([]byte, error) {
	parts, _, err := ooxmlParts(src, odfContentRE, nil, budget)
	if err != nil {
		return nil, err
	}
	if len(parts) != 1 {
		return nil, fmt.Errorf("no OpenDocument content")
	}
	return parts[0].data, nil
}

func odsText(src string, budget int64) (string, error) {
	data, err := odfContent(src, budget)
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	sheets := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "table" {
			continue
		}
		var sheet odfSheet
		if err := dec.DecodeElement(&sheet, &start); err != nil {
			return "", err
		}
		var rows [][]string
		for _, r := range sheet.Rows {
			var row []string
			for _, c := range r.Cells {
				paragraphs := make([]string, len(c.Paragraphs))
				for i, p := range c.Paragraphs {
					paragraphs[i] = string(p)
				}
				value := strings.Join(paragraphs, "\n")
				if value == "" {
					value = c.Value
				}
				repeat := max(1, c.Repeat)
				repeat = min(repeat, maxTableColumns-len(row))
				for i := 0; i < repeat; i++ {
					row = append(row, value)
				}
			}
			for len(row) > 0 && row[len(row)-1] == "" {
				row = row[:len(row)-1]
			}
			if len(row) == 0 {
				continue
			}
			for i := 0; i < max(1, r.Repeat) && len(rows) <= maxTableRows; i++ {
				rows = append(rows, row)
			}
			if len(rows) > maxTableRows {
				break
			}
		}
		sheets++
		fmt.Fprintf(&b, "## Sheet %d\n%s\n", sheets, markdownTable(rows))
		if b.Len() > maxTableBytes {
			return b.String(), nil
		}
	}
	return b.String(), nil
}

func odpText(src string, budget int64) (string, error) {
	data, err := odfContent(src, budget)
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	slide := 0
	inParagraph := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch v := tok.(type) {
		case xml.StartElement:
			switch v.Name.Local {
			case "page":
				slide++
				b.WriteString("\n## Slide " + strconv.Itoa(slide) + "\n")
			case "p":
				inParagraph = true
			}
		case xml.EndElement:
			if v.Name.Local == "p" {
				inParagraph = false
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inParagraph {
				b.Write(v)
			}
		}
	}
	return strings.TrimPrefix(b.String(), "\n"), nil
}
