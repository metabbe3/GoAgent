package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/metabbe3/GoAgent/internal/llm"
)

const (
	maxReportMarkdown   = 30000
	GeneratedFileMarker = "[generated-file: %s]"
)

var cjkRe = regexp.MustCompile(`[\p{Han}\p{Hangul}\p{Hiragana}\p{Katakana}]`)
var unsafeNameRe = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

func ensureRenderable(text string) error {
	if cjkRe.MatchString(text) {
		return fmt.Errorf("report contains Chinese/Japanese/Korean text, which this PDF generator's core fonts cannot render")
	}
	return nil
}

func (r *Registry) registerGenerateTools(dataDir string) {
	if dataDir == "" {
		dataDir = "data"
	}
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name: "generate_pdf",
			Description: "Render formatted markdown into an A4 PDF report with page numbers. " +
				"Core Latin-1 fonts only (CJK characters are rejected).",
			Parameters: schema(
				map[string]any{
					"title":    map[string]any{"type": "string", "description": "Report title"},
					"markdown": map[string]any{"type": "string", "description": "Markdown body text (# H1, ## H2, ### H3, - bullets, paragraphs)"},
				},
				[]string{"title", "markdown"},
			),
		},
	}, func(_ context.Context, argsJSON string) (string, error) {
		return handleGeneratePDF(dataDir, argsJSON)
	})
}

func handleGeneratePDF(dir, argsJSON string) (string, error) {
	var args struct {
		Title    string `json:"title"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if strings.TrimSpace(args.Markdown) == "" {
		return "", fmt.Errorf("markdown body is empty")
	}
	if len(args.Markdown) > maxReportMarkdown {
		args.Markdown = args.Markdown[:maxReportMarkdown]
	}

	if err := ensureRenderable(args.Title + "\n" + args.Markdown); err != nil {
		return "", err
	}

	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = "Report"
	}

	targetPath, err := exportPath(dir, title, "pdf")
	if err != nil {
		return "", err
	}

	pdfBytes, err := renderMarkdownToPDF(title, args.Markdown)
	if err != nil {
		return "", fmt.Errorf("pdf rendering failed: %w", err)
	}

	if err := os.WriteFile(targetPath, pdfBytes, 0o644); err != nil {
		return "", fmt.Errorf("failed to write pdf file: %w", err)
	}

	rel, err := filepath.Rel(".", targetPath)
	if err != nil {
		rel = targetPath
	}
	return fmt.Sprintf("PDF generated successfully: %s "+GeneratedFileMarker, rel, rel), nil
}

func exportPath(dir, title, ext string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(title))
	slug = unsafeNameRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "document"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}

	randBytes := make([]byte, 3)
	_, _ = rand.Read(randBytes)
	randSuffix := hex.EncodeToString(randBytes)

	fileName := fmt.Sprintf("%s-%s-%s.%s", time.Now().UTC().Format("20060102-150405"), randSuffix, slug, ext)
	subDir := filepath.Join(dir, "exports")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(subDir, fileName), nil
}

// renderMarkdownToPDF builds a pure Go stdlib multi-page PDF document.
func renderMarkdownToPDF(title, markdown string) ([]byte, error) {
	lines := strings.Split(markdown, "\n")

	// Page parameters (A4 size: 595.28 x 841.89 points)
	const (
		pageWidth    = 595.28
		pageHeight   = 841.89
		marginLeft   = 54.0
		marginTop    = 54.0
		marginBottom = 54.0
		usableHeight = pageHeight - marginTop - marginBottom
	)

	type textLine struct {
		font    string // "F1" (regular) or "F2" (bold)
		size    int
		text    string
		spacing float64
	}

	var allLines []textLine

	// Title header
	allLines = append(allLines, textLine{font: "F2", size: 18, text: title, spacing: 24})
	allLines = append(allLines, textLine{font: "F1", size: 9, text: "Generated on " + time.Now().UTC().Format("Mon Jan 2 15:04:05 2006 UTC"), spacing: 20})

	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			allLines = append(allLines, textLine{font: "F1", size: 10, text: "", spacing: 8})
			continue
		}
		if strings.HasPrefix(trimmed, "### ") {
			allLines = append(allLines, textLine{font: "F2", size: 12, text: trimmed[4:], spacing: 16})
		} else if strings.HasPrefix(trimmed, "## ") {
			allLines = append(allLines, textLine{font: "F2", size: 14, text: trimmed[3:], spacing: 18})
		} else if strings.HasPrefix(trimmed, "# ") {
			allLines = append(allLines, textLine{font: "F2", size: 16, text: trimmed[2:], spacing: 22})
		} else if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			bulletText := "• " + trimmed[2:]
			allLines = append(allLines, textLine{font: "F1", size: 10, text: bulletText, spacing: 14})
		} else {
			allLines = append(allLines, textLine{font: "F1", size: 10, text: trimmed, spacing: 14})
		}
	}

	// Paginate lines
	type pageContent struct {
		lines []textLine
	}
	var pages []pageContent
	var curPage []textLine
	curHeight := 0.0

	for _, l := range allLines {
		if curHeight+l.spacing > usableHeight && len(curPage) > 0 {
			pages = append(pages, pageContent{lines: curPage})
			curPage = nil
			curHeight = 0
		}
		curPage = append(curPage, l)
		curHeight += l.spacing
	}
	if len(curPage) > 0 || len(pages) == 0 {
		pages = append(pages, pageContent{lines: curPage})
	}

	// Construct PDF objects
	// Object 1: Catalog
	// Object 2: Pages
	// Object 3: Font F1 (Helvetica)
	// Object 4: Font F2 (Helvetica-Bold)
	// For each page:
	//   Object 5 + 2*i: Page
	//   Object 6 + 2*i: Content stream

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	var offsets []int
	offsets = append(offsets, 0) // dummy 0 index

	writeObj := func(id int, content string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", id, content)
	}

	numPages := len(pages)
	var kids []string
	for i := 0; i < numPages; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 5+2*i))
	}

	// Obj 1: Catalog
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")

	// Obj 2: Pages
	writeObj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), numPages))

	// Obj 3: F1
	writeObj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")

	// Obj 4: F2
	writeObj(4, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")

	// Pages and streams
	for i, pg := range pages {
		pageObjID := 5 + 2*i
		streamObjID := 6 + 2*i

		// Build stream
		var sb strings.Builder
		sb.WriteString("BT\n")
		y := pageHeight - marginTop
		for _, l := range pg.lines {
			y -= l.spacing
			if l.text == "" {
				continue
			}
			escaped := escapePDFString(l.text)
			fmt.Fprintf(&sb, "/%s %d Tf\n1 0 0 1 %.2f %.2f Tm\n(%s) Tj\n", l.font, l.size, marginLeft, y, escaped)
		}
		// Footer: Page N of Total
		footerText := fmt.Sprintf("Page %d of %d", i+1, numPages)
		fmt.Fprintf(&sb, "/F1 9 Tf\n1 0 0 1 %.2f 30.00 Tm\n(%s) Tj\n", pageWidth/2-30, escapePDFString(footerText))
		sb.WriteString("ET\n")

		streamBytes := sb.String()

		// Write Page Object
		writeObj(pageObjID, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Contents %d 0 R /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> >>", pageWidth, pageHeight, streamObjID))

		// Write Stream Object
		writeObj(streamObjID, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(streamBytes), streamBytes))
	}

	// Write xref
	xrefOffset := buf.Len()
	totalObjs := 4 + 2*numPages
	fmt.Fprintf(&buf, "xref\n0 %d\n", totalObjs+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= totalObjs; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}

	// Trailer
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", totalObjs+1, xrefOffset)

	return buf.Bytes(), nil
}

func escapePDFString(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == '(' || r == ')' || r == '\\' {
			sb.WriteRune('\\')
			sb.WriteRune(r)
		} else if r >= 32 && r <= 126 {
			sb.WriteRune(r)
		} else if r >= 160 && r <= 255 {
			sb.WriteRune(r)
		} else if r == '\n' || r == '\t' || r == '\r' {
			sb.WriteRune(' ')
		} else {
			sb.WriteRune('?')
		}
	}
	return sb.String()
}
