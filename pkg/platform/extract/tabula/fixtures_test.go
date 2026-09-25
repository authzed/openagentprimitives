package tabula_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
)

// These builders assemble the minimal valid bytes for each format this
// package registers, mirroring the shapes tabula's own test suite uses
// internally (github.com/tsawler/tabula/{docx,pptx}/reader_test.go).
// Fixtures are generated here rather than vendored so the repo carries no
// opaque binaries and every byte in a test is traceable to this file.
//
// buildXLSXWithCellRef is the exception: XLSX is not a registered MIME
// (see the package doc in tabula.go), so its fixture exists only to feed
// TestXLSX_NotRegistered — proof that the hostile shape it builds can
// never reach a parser today.

func zipFile(zw *zip.Writer, name, content string) {
	w, err := zw.Create(name)
	if err != nil {
		panic(err) // test-fixture construction; a failure here is a bug in the test itself
	}
	if _, err := w.Write([]byte(content)); err != nil {
		panic(err)
	}
}

func buildDOCX(bodyText string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zipFile(zw, "[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`)
	zipFile(zw, "_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`)
	zipFile(zw, "word/document.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body><w:p><w:r><w:t>`+bodyText+`</w:t></w:r></w:p></w:body>
</w:document>`)
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// buildPPTX builds a presentation with one slide per entry in slideTexts, so
// PageCount() reflects a real, checkable slide count.
func buildPPTX(slideTexts []string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	var overrides, sldIds, presRels strings.Builder
	for i := range slideTexts {
		n := i + 1
		fmt.Fprintf(&overrides, `<Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, n)
		fmt.Fprintf(&sldIds, `<p:sldId id="%d" r:id="rId%d"/>`, 255+n, n)
		fmt.Fprintf(&presRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`, n, n)
	}

	zipFile(zw, "[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
  `+overrides.String()+`
</Types>`)
	zipFile(zw, "_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
</Relationships>`)
	zipFile(zw, "ppt/_rels/presentation.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  `+presRels.String()+`
</Relationships>`)
	zipFile(zw, "ppt/presentation.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <p:sldIdLst>`+sldIds.String()+`</p:sldIdLst>
  <p:sldSz cx="9144000" cy="6858000"/>
</p:presentation>`)
	for i, text := range slideTexts {
		n := i + 1
		zipFile(zw, fmt.Sprintf("ppt/slides/slide%d.xml", n), `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld><p:spTree>
    <p:sp><p:txBody><a:p><a:r><a:t>`+text+`</a:t></a:r></a:p></p:txBody></p:sp>
  </p:spTree></p:cSld>
</p:sld>`)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// buildPDF assembles a single-page PDF from raw object bodies, with a
// correct classic xref table and trailer — the same shape tabula's own
// reader package uses to test itself (reader/extract_test.go: buildPDF).
func buildPDF(pageText string) []byte {
	stream := fmt.Sprintf("BT /F1 12 Tf 72 700 Td (%s) Tj ET", pageText)
	bodies := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(bodies))
	for i, body := range bodies {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(bodies)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(bodies)+1, xrefStart)
	return buf.Bytes()
}

// buildXLSXWithCellRef builds a single-sheet workbook whose one cell uses
// colRef as its column reference. Used only by TestXLSX_NotRegistered: this
// is the exact shape (a long run of "Z"s) that overflows int64 in tabula's
// xlsx.ColumnToIndex and panics, or, for a shorter run, drives an
// unrecoverable allocation hang — see tabula.go's package doc for the full
// finding. XLSX is not registered, so this fixture is never fed to
// Extract; the test exists to prove that stays true.
func buildXLSXWithCellRef(colRef string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zipFile(zw, "[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`)
	zipFile(zw, "_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`)
	zipFile(zw, "xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`)
	zipFile(zw, "xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets>
</workbook>`)
	zipFile(zw, "xl/worksheets/sheet1.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<sheetData><row r="1"><c r="`+colRef+`1"><v>1</v></c></row></sheetData>
</worksheet>`)
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// buildHighRatioDOCX wraps a highly repetitive payload (compresses at a
// disproportionate ratio, "zip-bomb-shaped") inside a minimal DOCX, so the
// fixture stays tiny on the wire while the parsed content is large.
func buildHighRatioDOCX(repeatWord string, repeatCount int) []byte {
	var body strings.Builder
	for range repeatCount {
		body.WriteString(repeatWord)
		body.WriteByte(' ')
	}
	return buildDOCX(body.String())
}
