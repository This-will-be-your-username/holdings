package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// A deliberately small .xlsx reader (standard library only): enough to read the first worksheet of an exported
// table as text - shared strings, inline strings, numbers - which is what broker and Vanguard exports contain.

func isXLSX(b []byte) bool { return len(b) > 4 && b[0] == 'P' && b[1] == 'K' && b[2] == 3 && b[3] == 4 }

type xlsxSheet struct {
	Rows []struct {
		Cells []struct {
			Ref string `xml:"r,attr"`
			T   string `xml:"t,attr"`
			V   string `xml:"v"`
			IS  struct {
				T []string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"is"`
		} `xml:"c"`
	} `xml:"sheetData>row"`
}

func colIndex(ref string) int {
	n := 0
	for _, c := range ref {
		if c >= 'A' && c <= 'Z' {
			n = n*26 + int(c-'A') + 1
		} else if c >= 'a' && c <= 'z' {
			n = n*26 + int(c-'a') + 1
		} else {
			break
		}
	}
	return n - 1
}

func readZipFile(files map[string]*zip.File, name string) ([]byte, error) {
	f := files[name]
	if f == nil {
		return nil, fmt.Errorf("missing %s", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 64<<20))
}

// readXLSX returns the rows of the first worksheet. Dates stored as real Excel dates come back as serial numbers
// (the price importer converts them); dates stored as text come back as written.
func readXLSX(b []byte) ([][]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("not a readable .xlsx file: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}

	sheetPath := "xl/worksheets/sheet1.xml"
	if wbRaw, err := readZipFile(files, "xl/workbook.xml"); err == nil {
		var wb struct {
			Sheets []struct {
				RID string `xml:"id,attr"`
			} `xml:"sheets>sheet"`
		}
		var rels struct {
			R []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		relRaw, _ := readZipFile(files, "xl/_rels/workbook.xml.rels")
		if xml.Unmarshal(wbRaw, &wb) == nil && xml.Unmarshal(relRaw, &rels) == nil && len(wb.Sheets) > 0 {
			for _, r := range rels.R {
				if r.ID == wb.Sheets[0].RID {
					t := strings.TrimPrefix(r.Target, "/")
					if !strings.HasPrefix(t, "xl/") {
						t = "xl/" + t
					}
					sheetPath = t
				}
			}
		}
	}

	var shared []string
	if raw, err := readZipFile(files, "xl/sharedStrings.xml"); err == nil {
		var sst struct {
			SI []struct {
				T string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if xml.Unmarshal(raw, &sst) == nil {
			for _, si := range sst.SI {
				s := si.T
				for _, r := range si.R {
					s += r.T
				}
				shared = append(shared, s)
			}
		}
	}

	raw, err := readZipFile(files, sheetPath)
	if err != nil {
		return nil, fmt.Errorf("worksheet not found in the .xlsx file")
	}
	var sh xlsxSheet
	if err := xml.Unmarshal(raw, &sh); err != nil {
		return nil, fmt.Errorf("could not read the worksheet: %w", err)
	}
	var rows [][]string
	for _, r := range sh.Rows {
		var row []string
		for i, c := range r.Cells {
			col := colIndex(c.Ref)
			if col < 0 {
				col = i
			}
			for len(row) <= col {
				row = append(row, "")
			}
			var v string
			switch c.T {
			case "s":
				var idx int
				if _, err := fmt.Sscanf(c.V, "%d", &idx); err == nil && idx >= 0 && idx < len(shared) {
					v = shared[idx]
				}
			case "inlineStr":
				v = strings.Join(c.IS.T, "")
				for _, r := range c.IS.R {
					v += r.T
				}
			default: // "str", "n", "b", "" ...
				v = c.V
			}
			row[col] = strings.TrimSpace(v)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// xlsxToCSV renders the first worksheet as CSV text so the same parsers handle both formats.
func xlsxToCSV(b []byte) ([]byte, error) {
	rows, err := readXLSX(b)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	for _, r := range rows {
		_ = w.Write(r)
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}
