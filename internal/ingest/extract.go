package ingest

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// MaxExtractChars caps what is fed to the LLM from one file.
const MaxExtractChars = 48_000

// MaxRowsPerSheet caps row streaming per spreadsheet sheet.
const MaxRowsPerSheet = 5_000

var tagRe = regexp.MustCompile(`<[^>]*>`)
var cjkRe = regexp.MustCompile(`[\p{Han}\p{Hangul}\p{Hiragana}\p{Katakana}]`)

// collapseCellBreaks converts paragraph breaks inside a cell to a space.
func collapseCellBreaks(s string) string {
	for strings.Contains(s, "\n\t") {
		s = strings.ReplaceAll(s, "\n\t", " \t")
	}
	return s
}

// ExtractText returns plain text from supported document formats.
// Supported: .txt, .md, .csv, .json, .log, .xml (passthrough), .docx, .xlsx, .pdf.
func ExtractText(filename string, data []byte) (string, error) {
	switch strings.ToLower(path.Ext(filename)) {
	case ".txt", ".md", ".csv", ".json", ".log", ".xml", "":
		return string(data), nil
	case ".docx":
		return extractDocx(data)
	case ".xlsx":
		return extractXlsx(data)
	case ".pdf":
		return extractPdf(data)
	default:
		return "", fmt.Errorf("unsupported file type %q (supported: txt, md, csv, json, log, xml, docx, xlsx, pdf)", path.Ext(filename))
	}
}

// extractDocx extracts text from word/document.xml in a DOCX archive.
func extractDocx(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open docx: %w", err)
	}

	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()

		raw, err := io.ReadAll(io.LimitReader(rc, 20<<20))
		if err != nil {
			return "", err
		}

		withBreaks := strings.ReplaceAll(string(raw), "</w:p>", "\n")
		withBreaks = strings.ReplaceAll(withBreaks, "</w:tc>", "\t")
		withBreaks = strings.ReplaceAll(withBreaks, "</w:tr>", "\n")
		text := tagRe.ReplaceAllString(withBreaks, "")
		text = collapseCellBreaks(text)
		return strings.TrimSpace(html.UnescapeString(text)), nil
	}

	return "", fmt.Errorf("docx has no word/document.xml")
}

// extractXlsx extracts text from worksheets in an XLSX archive using stdlib archive/zip and encoding/xml.
func extractXlsx(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open xlsx: %w", err)
	}

	// 1. Read shared strings if present
	var sharedStrings []string
	for _, f := range zr.File {
		if f.Name == "xl/sharedStrings.xml" {
			rc, err := f.Open()
			if err == nil {
				sharedStrings = parseSharedStrings(rc)
				rc.Close()
			}
			break
		}
	}

	// 2. Read sheet names from workbook.xml if present
	sheetNames := make(map[string]string)
	var sheetOrder []string
	for _, f := range zr.File {
		if f.Name == "xl/workbook.xml" {
			rc, err := f.Open()
			if err == nil {
				sheetNames, sheetOrder = parseWorkbookSheets(rc)
				rc.Close()
			}
			break
		}
	}

	// Find worksheet files
	sheetFiles := make(map[string]*zip.File)
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/sheet") && strings.HasSuffix(f.Name, ".xml") {
			base := path.Base(f.Name)
			sheetFiles[base] = f
		}
	}

	if len(sheetFiles) == 0 {
		return "", fmt.Errorf("xlsx has no worksheets")
	}

	// Build ordered list of sheets to parse
	type sheetItem struct {
		name string
		file *zip.File
	}
	var orderedSheets []sheetItem

	if len(sheetOrder) > 0 {
		for i, name := range sheetOrder {
			fileName := fmt.Sprintf("sheet%d.xml", i+1)
			if zf, ok := sheetFiles[fileName]; ok {
				orderedSheets = append(orderedSheets, sheetItem{name: name, file: zf})
			}
		}
	}

	if len(orderedSheets) == 0 {
		for i := 1; i <= 100; i++ {
			fileName := fmt.Sprintf("sheet%d.xml", i)
			if zf, ok := sheetFiles[fileName]; ok {
				name := sheetNames[fileName]
				if name == "" {
					name = fmt.Sprintf("Sheet%d", i)
				}
				orderedSheets = append(orderedSheets, sheetItem{name: name, file: zf})
			}
		}
	}

	var sb strings.Builder
	for _, item := range orderedSheets {
		rc, err := item.file.Open()
		if err != nil {
			continue
		}
		rows, err := parseWorksheetRows(rc, sharedStrings, MaxRowsPerSheet)
		rc.Close()
		if err != nil || len(rows) == 0 {
			continue
		}

		fmt.Fprintf(&sb, "Sheet: %s\n", item.name)
		for _, row := range rows {
			joined := strings.TrimRight(strings.Join(row, "\t"), "\t")
			if strings.TrimSpace(joined) == "" {
				continue
			}
			sb.WriteString(joined)
			sb.WriteByte('\n')
			if sb.Len() > MaxExtractChars {
				fmt.Fprintf(&sb, "...[extraction capped at %d chars]\n", MaxExtractChars)
				return strings.TrimSpace(sb.String()), nil
			}
		}
		sb.WriteByte('\n')
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("xlsx has no readable cells (sheet may be charts-only)")
	}
	return out, nil
}

func parseSharedStrings(r io.Reader) []string {
	var out []string
	decoder := xml.NewDecoder(r)
	var inT bool
	var curText strings.Builder

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local == "si" {
				curText.Reset()
			} else if el.Name.Local == "t" {
				inT = true
			}
		case xml.EndElement:
			if el.Name.Local == "t" {
				inT = false
			} else if el.Name.Local == "si" {
				out = append(out, curText.String())
			}
		case xml.CharData:
			if inT {
				curText.WriteString(string(el))
			}
		}
	}
	return out
}

func parseWorkbookSheets(r io.Reader) (map[string]string, []string) {
	names := make(map[string]string)
	var order []string
	decoder := xml.NewDecoder(r)
	sheetIdx := 1

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			var name string
			for _, attr := range se.Attr {
				if attr.Name.Local == "name" {
					name = attr.Value
					break
				}
			}
			if name != "" {
				fileName := fmt.Sprintf("sheet%d.xml", sheetIdx)
				names[fileName] = name
				order = append(order, name)
				sheetIdx++
			}
		}
	}
	return names, order
}

func parseWorksheetRows(r io.Reader, sharedStrings []string, maxRows int) ([][]string, error) {
	var rows [][]string
	decoder := xml.NewDecoder(r)

	var inCell bool
	var inV bool
	var inIsT bool
	var cellType string
	var curVal strings.Builder
	var curRow []string

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "row":
				curRow = curRow[:0]
			case "c":
				inCell = true
				cellType = ""
				for _, attr := range el.Attr {
					if attr.Name.Local == "t" {
						cellType = attr.Value
						break
					}
				}
				curVal.Reset()
			case "v":
				if inCell {
					inV = true
				}
			case "t":
				if inCell {
					inIsT = true
				}
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "v":
				inV = false
			case "t":
				inIsT = false
			case "c":
				inCell = false
				val := curVal.String()
				if cellType == "s" {
					if idx, err := strconv.Atoi(val); err == nil && idx >= 0 && idx < len(sharedStrings) {
						val = sharedStrings[idx]
					}
				}
				curRow = append(curRow, val)
			case "row":
				if len(curRow) > 0 {
					rowCopy := make([]string, len(curRow))
					copy(rowCopy, curRow)
					rows = append(rows, rowCopy)
					if len(rows) >= maxRows {
						return rows, nil
					}
				}
			}
		case xml.CharData:
			if inV || inIsT {
				curVal.WriteString(string(el))
			}
		}
	}
	return rows, nil
}

var pdfStreamRe = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
var pdfTjRe = regexp.MustCompile(`\((.*?)\)\s*Tj`)
var pdfTJRe = regexp.MustCompile(`\[(.*?)\]\s*TJ`)
var pdfInnerParenRe = regexp.MustCompile(`\((.*?)\)`)

// extractPdf extracts Latin-1 text layer from PDF and guards against non-Latin/CJK characters.
func extractPdf(data []byte) (string, error) {
	if !bytes.HasPrefix(data, []byte("%PDF-")) && !bytes.Contains(data[:min(512, len(data))], []byte("%PDF-")) {
		return "", fmt.Errorf("invalid pdf header")
	}

	// Guard: check if raw data has CJK
	if cjkRe.Match(data) {
		return "", fmt.Errorf("cannot render: document contains non-Latin/CJK characters")
	}

	var sb strings.Builder
	// Find all streams
	matches := pdfStreamRe.FindAllSubmatch(data, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		rawStream := m[1]
		var textBytes []byte

		// Try zlib decompress
		zr, err := zlib.NewReader(bytes.NewReader(rawStream))
		if err == nil {
			decomp, err := io.ReadAll(zr)
			zr.Close()
			if err == nil && len(decomp) > 0 {
				textBytes = decomp
			} else {
				textBytes = rawStream
			}
		} else {
			textBytes = rawStream
		}

		extracted := extractPdfStreamText(string(textBytes))
		if extracted != "" {
			sb.WriteString(extracted)
			sb.WriteByte('\n')
		}
	}

	// Also scan plain text outside streams for BT ... ET blocks
	directText := extractPdfStreamText(string(data))
	if directText != "" && sb.Len() == 0 {
		sb.WriteString(directText)
	}

	out := strings.TrimSpace(sb.String())
	if cjkRe.MatchString(out) {
		return "", fmt.Errorf("cannot render: document contains non-Latin/CJK characters")
	}

	return out, nil
}

func extractPdfStreamText(s string) string {
	var sb strings.Builder

	// Look for (text) Tj
	for _, m := range pdfTjRe.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 {
			sb.WriteString(cleanPdfStr(m[1]))
			sb.WriteByte(' ')
		}
	}

	// Look for [(text) 10 (text)] TJ
	for _, m := range pdfTJRe.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 {
			inner := m[1]
			for _, im := range pdfInnerParenRe.FindAllStringSubmatch(inner, -1) {
				if len(im) > 1 {
					sb.WriteString(cleanPdfStr(im[1]))
				}
			}
			sb.WriteByte(' ')
		}
	}

	return strings.TrimSpace(sb.String())
}

func cleanPdfStr(s string) string {
	s = strings.ReplaceAll(s, `\\`, `\`)
	s = strings.ReplaceAll(s, `\(`, `(`)
	s = strings.ReplaceAll(s, `\)`, `)`)
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\t`, "\t")
	return s
}

// Truncate clamps extracted text to MaxExtractChars with an explicit marker.
func Truncate(s string) string {
	if len(s) <= MaxExtractChars {
		return s
	}
	r := []rune(s)
	if len(r) <= MaxExtractChars {
		return s
	}
	return string(r[:MaxExtractChars]) + "\n…[truncated]"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
