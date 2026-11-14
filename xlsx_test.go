package main

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// makeXLSX builds a minimal workbook around the given sheet XML (and optional shared strings).
func makeXLSX(t *testing.T, sheetXML, sharedStrings string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	add("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`)
	add("xl/workbook.xml", `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="data" sheetId="1" r:id="rId1"/></sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="worksheet" Target="worksheets/sheet1.xml"/></Relationships>`)
	if sharedStrings != "" {
		add("xl/sharedStrings.xml", `<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+sharedStrings+`</sst>`)
	}
	add("xl/worksheets/sheet1.xml", `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`+sheetXML+`</sheetData></worksheet>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// text cells written the way Vanguard's own export stores them (t="str"), mirroring the real file's layout
func vanguardLikeSheet() string {
	row := func(n int, a, b, c, d string) string {
		cell := func(col, v string) string { return `<c r="` + col + itoa(n) + `" t="str"><v>` + v + `</v></c>` }
		return `<row r="` + itoa(n) + `">` + cell("A", a) + cell("B", b) + cell("C", c) + cell("D", d) + `</row>`
	}
	return row(1, "Date", "Buy", "Sell", "NAV") +
		row(2, "12 Nov 2026", "$1.5673", "$1.5671", "$1.5673") +
		row(3, "10 Nov 2026", "$1.5641", "$1.5634", "$1.5641") +
		row(4, "09 Nov 2026", "$1.5601", "$1.5598", "$1.5601") +
		row(5, "19 Nov 2014", "$1.3278", "$1.3261", "$1.3261")
}

func itoa(n int) string {
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestReadXLSXAndParseVanguardExport(t *testing.T) {
	b := makeXLSX(t, vanguardLikeSheet(), "")
	if !isXLSX(b) {
		t.Fatal("magic")
	}
	rows, err := readXLSX(b)
	if err != nil || len(rows) != 5 || rows[0][3] != "NAV" || rows[1][0] != "12 Nov 2026" || rows[1][1] != "$1.5673" {
		t.Fatalf("%v %v", rows, err)
	}
	pi, err := ParsePriceFile(b)
	if err != nil {
		t.Fatal(err)
	}
	// NAV is preferred over buy / sell
	if pi.Column != "NAV" || pi.Rows != 4 || pi.From != "2014-11-19" || pi.To != "2026-11-12" {
		t.Errorf("%+v", pi)
	}
	if pi.Prices["2026-11-12"] != 1.5673 || pi.Prices["2014-11-19"] != 1.3261 || pi.Prices["2026-11-10"] != 1.5641 {
		t.Errorf("%v", pi.Prices)
	}
}

func TestSharedStringsAndExcelSerialDates(t *testing.T) {
	ss := `<si><t>Date</t></si><si><t>Unit price</t></si><si><t>Exit price</t></si>`
	sheet := `<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c></row>` +
		`<row r="2"><c r="A2"><v>46338</v></c><c r="B2"><v>1.5673</v></c><c r="C2"><v>1.5671</v></c></row>` + // 12 Nov 2026
		`<row r="3"><c r="A3"><v>46336</v></c><c r="B3"><v>1.5601</v></c><c r="C3"><v>1.5598</v></c></row>` +
		`<row r="4"><c r="A4"><v>46335</v></c><c r="C4"><v>1.55</v></c></row>` // price missing in the preferred column
	pi, err := ParsePriceFile(makeXLSX(t, sheet, ss))
	if err != nil {
		t.Fatal(err)
	}
	if pi.Column != "unit price" || pi.Rows != 2 || pi.Skipped != 1 || pi.Prices["2026-11-12"] != 1.5673 || pi.Prices["2026-11-10"] != 1.5601 {
		t.Errorf("%+v %v", pi, pi.Prices)
	}
}

func TestInlineStringsAndSparseColumns(t *testing.T) {
	sheet := `<row r="1"><c r="B1" t="inlineStr"><is><t>Date</t></is></c><c r="D1" t="inlineStr"><is><t>NAV</t></is></c></row>` +
		`<row r="2"><c r="B2" t="inlineStr"><is><t>2026-11-12</t></is></c><c r="D2"><v>2.5</v></c></row>`
	rows, err := readXLSX(makeXLSX(t, sheet, ""))
	if err != nil || len(rows[0]) != 4 || rows[0][1] != "Date" || rows[0][3] != "NAV" || rows[0][0] != "" {
		t.Fatalf("%v %v", rows, err)
	}
	pi, err := ParsePriceHistory(rows)
	if err != nil || pi.Prices["2026-11-12"] != 2.5 {
		t.Errorf("%v %v", pi, err)
	}
}

func TestPriceHistoryFromCSVAndPreferences(t *testing.T) {
	// no NAV or unit price: the sell (exit) price is preferred to the buy (entry) price
	pi, err := ParsePriceFile([]byte("Date,Buy,Sell\n12 Nov 2026,$1.5673,$1.5671\n10 Nov 2026,$1.5641,$1.5634\n"))
	if err != nil || pi.Column != "sell (exit) price" || pi.Prices["2026-11-12"] != 1.5671 {
		t.Errorf("%+v %v", pi, err)
	}
	// day-first dates are detected from the data (25/03 can only be day-first)
	pi, err = ParsePriceFile([]byte("Date,NAV\n03/04/2026,1.50\n25/03/2026,1.40\n"))
	if err != nil || pi.Prices["2026-04-03"] != 1.50 || pi.Prices["2026-03-25"] != 1.40 {
		t.Errorf("%+v %v", pi, err)
	}
	// no header at all: a date followed by a price
	pi, err = ParsePriceFile([]byte("2026-11-12,1.5673\n2026-11-10,1.5641\n"))
	if err != nil || pi.Rows != 2 || pi.Column != "second column" {
		t.Errorf("%+v %v", pi, err)
	}
	// preamble before the header, and junk rows
	pi, err = ParsePriceFile([]byte("Vanguard Balanced Index Fund\nExported 14 Nov 2026\n\nDate,Buy,Sell,NAV\n12 Nov 2026,$1.5673,$1.5671,$1.5673\nnot a date,x,y,z\n10 Nov 2026,$0,$0,$0\n"))
	if err != nil || pi.Rows != 1 || pi.Skipped != 2 || pi.Column != "NAV" {
		t.Errorf("%+v %v", pi, err)
	}
}

func TestPriceFileErrors(t *testing.T) {
	for name, in := range map[string][]byte{
		"no price column": []byte("Date,Comment\n12 Nov 2026,hello\n"),
		"no usable rows":  []byte("Date,NAV\nfoo,bar\n"),
		"empty":           []byte(""),
		"not a workbook":  []byte("PK\x03\x04garbage"),
	} {
		if _, err := ParsePriceFile(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestTransactionImporterAcceptsASpreadsheetToo(t *testing.T) {
	var sheet strings.Builder
	rows := [][]string{
		{"Code", "Company", "Date", "Type", "Quantity", "Unit Price ($)", "Brokerage+GST ($)"},
		{"VGS", "VANGUARD", "02/01/2025", "Buy", "10", "100.50", "9.50"},
	}
	for i, r := range rows {
		sheet.WriteString(`<row r="` + itoa(i+1) + `">`)
		for j, v := range r {
			sheet.WriteString(`<c r="` + string(rune('A'+j)) + itoa(i+1) + `" t="str"><v>` + v + `</v></c>`)
		}
		sheet.WriteString(`</row>`)
	}
	csvData, err := xlsxToCSV(makeXLSX(t, sheet.String(), ""))
	if err != nil {
		t.Fatal(err)
	}
	an := analyze(t, string(csvData))
	ts := okTxns(an)
	if len(ts) != 1 || ts[0].Symbol != "VGS.AX" || ts[0].Units != 10 || ts[0].Fees != 9.5 || ts[0].Date != "2025-01-02" {
		t.Errorf("%+v", an.Rows)
	}
}
