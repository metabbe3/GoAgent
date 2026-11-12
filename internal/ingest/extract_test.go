package ingest

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestExtractTextPlain(t *testing.T) {
	out, err := ExtractText("notes.txt", []byte("hello world"))
	if err != nil || out != "hello world" {
		t.Fatalf("txt: %q, %v", out, err)
	}
	out, err = ExtractText("report.md", []byte("# hi"))
	if err != nil || out != "# hi" {
		t.Fatalf("md: %q, %v", out, err)
	}
}

func TestExtractDocx(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte(`<w:document><w:body><w:p>Hello <w:r><w:t>world</w:t></w:r></w:p><w:p>Second para</w:p></w:body></w:document>`))
	zw.Close()

	out, err := ExtractText("doc.docx", buf.Bytes())
	if err != nil {
		t.Fatalf("docx: %v", err)
	}
	if out != "Hello world\nSecond para" {
		t.Fatalf("docx text = %q", out)
	}
}

func TestExtractDocxTableCellsGetSeparators(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte("<w:document><w:body><w:tbl><w:tr><w:tc><w:p>Alpha</w:p></w:tc><w:tc><w:p>Beta</w:p></w:tc></w:tr></w:tbl></w:body></w:document>"))
	zw.Close()

	out, err := extractDocx(buf.Bytes())
	if err != nil {
		t.Fatalf("extractDocx: %v", err)
	}
	if !strings.Contains(out, "Alpha \tBeta") {
		t.Errorf("table cells not tab-separated: %q", out)
	}
}

func TestExtractXlsxStdlibTwoSheets(t *testing.T) {
	// Build minimal XLSX with 2 sheets and sharedStrings
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// xl/workbook.xml
	w, _ := zw.Create("xl/workbook.xml")
	w.Write([]byte(`<workbook><sheets><sheet name="Primary" sheetId="1" r:id="rId1"/><sheet name="Secondary" sheetId="2" r:id="rId2"/></sheets></workbook>`))

	// xl/sharedStrings.xml
	w, _ = zw.Create("xl/sharedStrings.xml")
	w.Write([]byte(`<sst><si><t>Item A</t></si><si><t>Item B</t></si><si><t>Category 1</t></si></sst>`))

	// xl/worksheets/sheet1.xml
	w, _ = zw.Create("xl/worksheets/sheet1.xml")
	w.Write([]byte(`<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>2</v></c></row></sheetData></worksheet>`))

	// xl/worksheets/sheet2.xml
	w, _ = zw.Create("xl/worksheets/sheet2.xml")
	w.Write([]byte(`<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>1</v></c><c r="B1"><v>999</v></c></row></sheetData></worksheet>`))

	zw.Close()

	out, err := extractXlsx(buf.Bytes())
	if err != nil {
		t.Fatalf("extractXlsx failed: %v", err)
	}

	if !strings.Contains(out, "Sheet: Primary") || !strings.Contains(out, "Item A\tCategory 1") {
		t.Errorf("sheet 1 content missing: %q", out)
	}
	if !strings.Contains(out, "Sheet: Secondary") || !strings.Contains(out, "Item B\t999") {
		t.Errorf("sheet 2 content missing: %q", out)
	}
}

func TestExtractPdfCJKGuardReject(t *testing.T) {
	// PDF with CJK characters should be rejected
	cjkPdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n(你好世界) Tj\n")
	_, err := extractPdf(cjkPdf)
	if err == nil {
		t.Fatal("expected error for CJK pdf, got nil")
	}
	if !strings.Contains(err.Error(), "cannot render") {
		t.Errorf("expected 'cannot render' in error, got %v", err)
	}
}

func TestExtractPdfLatin1Valid(t *testing.T) {
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\nstream\n(Hello GoMesh) Tj\nendstream\n")
	out, err := extractPdf(pdf)
	if err != nil {
		t.Fatalf("extractPdf failed: %v", err)
	}
	if !strings.Contains(out, "Hello GoMesh") {
		t.Errorf("expected 'Hello GoMesh', got %q", out)
	}
}

func TestExtractScannedPdfReturnsEmpty(t *testing.T) {
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n3 0 obj\n<< /Type /Page /Parent 2 0 R >>\nendobj\nxref\n0 4\n0000000000 65535 f \ntrailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n100\n%%EOF\n")
	out, err := extractPdf(pdf)
	if err != nil {
		t.Fatalf("scanned pdf should not return error: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("scanned pdf expected empty text, got %q", out)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate(strings.Repeat("a", MaxExtractChars+5)); !strings.HasSuffix(got, "…[truncated]") {
		t.Fatal("truncation marker missing")
	}
	if got := Truncate("short"); got != "short" {
		t.Fatalf("short text altered: %q", got)
	}
}
