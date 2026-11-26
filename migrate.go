package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Export and import of a complete installation, for moving to another computer, container or server.
//
// The export is one JSON file (a "backup bundle") holding the whole data file: transactions, instruments, saved and
// manual prices, distributions, exchange rates, import log, tax settings, owners, targets and private-company
// checkpoints. The Telegram bot settings are included only when asked for, because the bot token is a secret.
//
// Import replaces the current data with the bundle's. The data file being replaced is first copied to a
// timestamped .bak file beside it, so an import can always be undone. A plain data file (fundtrack.json copied from
// another installation) is accepted too, as is either one gzipped.

const (
	bundleFormat  = "holdings-backup"
	bundleVersion = 1
	maxBundle     = 256 << 20 // largest file accepted for import
)

type Bundle struct {
	Format     string          `json:"format"`
	Version    int             `json:"version"`
	ExportedAt string          `json:"exported_at"`
	Mode       string          `json:"mode,omitempty"` // "real" or "test": which dataset it came from
	Data       json.RawMessage `json:"data"`
	Telegram   *TelegramConfig `json:"telegram,omitempty"`
}

// BundleSummary describes what a bundle holds, for messages and the CLI.
type BundleSummary struct {
	Txns, Instruments, Imports, Owners, PriceSeries int
	First, Last                                     string // date range of the transactions
}

func (s BundleSummary) String() string {
	out := fmt.Sprintf("%d transactions, %d instruments, %d price series, %d owners, %d import records",
		s.Txns, s.Instruments, s.PriceSeries, s.Owners, s.Imports)
	if s.First != "" {
		out += fmt.Sprintf(" (transactions %s to %s)", s.First, s.Last)
	}
	return out
}

func summarise(db *DB) BundleSummary {
	s := BundleSummary{Txns: len(db.Txns), Instruments: len(db.Instruments), Imports: len(db.Imports), Owners: len(db.Owners),
		PriceSeries: len(db.Prices) + len(db.Manual)}
	for _, t := range db.Txns {
		if s.First == "" || t.Date < s.First {
			s.First = t.Date
		}
		if t.Date > s.Last {
			s.Last = t.Date
		}
	}
	return s
}

// WriteBundle writes a complete export of db (and tg, if not nil) to w.
func WriteBundle(w io.Writer, db *DB, mode string, tg *TelegramConfig, now time.Time) error {
	data, err := json.Marshal(db)
	if err != nil {
		return err
	}
	b := Bundle{Format: bundleFormat, Version: bundleVersion, ExportedAt: now.UTC().Format(time.RFC3339), Mode: mode, Data: data}
	if tg != nil && (tg.Token != "" || tg.ChatID != 0) {
		c := *tg
		c.PendingChat, c.PendingName = 0, ""
		b.Telegram = &c
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	return enc.Encode(&b)
}

// ReadBundle parses an export (or a plain data file, optionally gzipped) and returns a ready-to-use DB.
func ReadBundle(r io.Reader) (*DB, *Bundle, error) {
	br := bufio.NewReader(io.LimitReader(r, maxBundle+1))
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, fmt.Errorf("not a valid gzip file: %w", err)
		}
		defer zr.Close()
		br = bufio.NewReader(io.LimitReader(zr, maxBundle+1))
	}
	raw, err := io.ReadAll(br)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maxBundle {
		return nil, nil, fmt.Errorf("file is larger than %d MB", maxBundle>>20)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // UTF-8 byte order mark
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, errors.New("the file is empty")
	}

	var head struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, nil, fmt.Errorf("not a Holdings export or data file (%v)", err)
	}

	var b Bundle
	data := raw
	if head.Format != "" {
		if head.Format != bundleFormat {
			return nil, nil, fmt.Errorf("unknown export format %q", head.Format)
		}
		if head.Version > bundleVersion {
			return nil, nil, fmt.Errorf("this export was made by a newer version of Holdings (format %d, this version reads up to %d): upgrade first", head.Version, bundleVersion)
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, nil, fmt.Errorf("damaged export file: %w", err)
		}
		if len(b.Data) == 0 || string(b.Data) == "null" {
			return nil, nil, errors.New("the export file holds no data")
		}
		data = b.Data
	} else {
		// A plain data file: it must at least look like one.
		var probe map[string]json.RawMessage
		_ = json.Unmarshal(raw, &probe)
		_, hasTxns := probe["txns"]
		_, hasInst := probe["instruments"]
		if !hasTxns && !hasInst {
			return nil, nil, errors.New("not a Holdings export or data file (no transactions or instruments found)")
		}
	}

	var db DB
	if err := json.Unmarshal(data, &db); err != nil {
		return nil, nil, fmt.Errorf("damaged data: %w", err)
	}
	db.init()
	if err := checkDB(&db); err != nil {
		return nil, nil, err
	}
	for i := range db.Txns {
		if db.Txns[i].FP == "" {
			db.Txns[i].FP = Fingerprint(db.Txns[i])
		}
	}
	return &db, &b, nil
}

// checkDB catches data that would break the app rather than merely look odd.
func checkDB(db *DB) error {
	seen := map[string]bool{}
	for i, t := range db.Txns {
		if t.ID == "" {
			db.Txns[i].ID = newID()
		} else if seen[t.ID] {
			return fmt.Errorf("transaction id %q appears twice", t.ID)
		}
		seen[db.Txns[i].ID] = true
		if _, err := time.Parse(dateFmt, t.Date); err != nil {
			return fmt.Errorf("transaction %s has a bad date %q", t.ID, t.Date)
		}
		if t.Symbol == "" {
			return fmt.Errorf("transaction %s on %s has no symbol", t.ID, t.Date)
		}
	}
	for sym, in := range db.Instruments {
		if in == nil {
			delete(db.Instruments, sym)
		}
	}
	return nil
}

// Replace swaps the store's data for db, keeping a copy of the current data file first. It returns the backup path
// ("" when there was no data file yet).
func (s *Store) Replace(db *DB, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup, err := s.backupLocked(now)
	if err != nil {
		return "", err
	}
	quotes, live := s.db.Quotes, s.db.Live
	s.db = *db
	s.db.init()
	s.db.Quotes, s.db.Live = quotes, live
	s.db.isView, s.db.viewOwner = false, ""
	return backup, s.saveLocked()
}

func (s *Store) backupLocked(now time.Time) (string, error) {
	src, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	dst := fmt.Sprintf("%s.%s.bak", s.path, now.Format("20060102-150405"))
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		return "", fmt.Errorf("couldn't keep a copy of the current data, nothing was changed: %w", err)
	}
	return dst, nil
}

// ---------- web ----------

func (a *App) registerMigrate(mux *http.ServeMux) {
	mux.HandleFunc("GET /export/backup.json", a.exportBackup)
	mux.HandleFunc("POST /settings/import", a.importBackup)
}

func (a *App) exportBackup(w http.ResponseWriter, r *http.Request) {
	var tg *TelegramConfig
	if r.URL.Query().Get("telegram") != "" {
		a.tg.mu.Lock()
		c := a.tg.cfg
		a.tg.mu.Unlock()
		tg = &c
	}
	mode, name := "real", "holdings-backup"
	if a.inTest() {
		mode, name = "test", "holdings-test-backup"
	}
	var buf bytes.Buffer
	var err error
	a.st().View(func(db *DB) { err = WriteBundle(&buf, db, mode, tg, time.Now()) })
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.json"`, name, time.Now().Format(dateFmt)))
	_, _ = w.Write(buf.Bytes())
}

func (a *App) importBackup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBundle+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		redirect(w, r, "/settings#migrate", "", "Couldn't read the upload: "+err.Error())
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		redirect(w, r, "/settings#migrate", "", "Choose an export file to import.")
		return
	}
	defer f.Close()
	if r.FormValue("confirm") == "" {
		redirect(w, r, "/settings#migrate", "", "Tick the box to confirm that the import replaces your current data.")
		return
	}
	db, b, err := ReadBundle(f)
	if err != nil {
		redirect(w, r, "/settings#migrate", "", "Nothing was imported: "+err.Error())
		return
	}
	backup, err := a.st().Replace(db, time.Now())
	if err != nil {
		redirect(w, r, "/settings#migrate", "", "Nothing was imported: "+err.Error())
		return
	}
	msg := "Imported " + summarise(db).String() + "."
	if a.inTest() {
		msg += " (into the test dataset, because test mode is on)"
	}
	if b.Telegram != nil && r.FormValue("telegram") != "" {
		a.tg.mu.Lock()
		a.tg.cfg = *b.Telegram
		err := a.tg.saveLocked()
		a.tg.mu.Unlock()
		if err != nil {
			msg += " Telegram settings couldn't be saved: " + err.Error()
		} else {
			a.setTGStatus("", "", false)
			a.startTelegram()
			msg += " Telegram settings restored."
		}
	}
	if backup != "" {
		msg += " Your previous data was kept as " + filepath.Base(backup) + "."
	}
	redirect(w, r, "/settings#migrate", msg, "")
}

// ---------- command line ----------

// runCLI handles "holdings export" and "holdings import". It reports whether args named a command.
func runCLI(args []string, dataFile string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	usage := func() {
		fmt.Fprintln(stderr, `usage:
  holdings                         run the web app
  holdings export [-telegram] [FILE]   write a complete export to FILE (default: standard output)
  holdings import [-telegram] FILE     replace the data with an export or data file (FILE "-" = standard input)

Stop the web app before importing. DATA_FILE selects the data file (default data/fundtrack.json).`)
	}
	cmd, rest := args[0], args[1:]
	withTG := false
	var files []string
	for _, a := range rest {
		switch a {
		case "-telegram", "--telegram":
			withTG = true
		case "-h", "--help":
			usage()
			return true, 0
		default:
			files = append(files, a)
		}
	}
	tg := func() *tgState { var s tgState; s.load(filepath.Dir(dataFile)); return &s }

	switch cmd {
	case "export":
		if len(files) > 1 {
			usage()
			return true, 2
		}
		s, err := OpenStore(dataFile)
		if err != nil {
			fmt.Fprintln(stderr, "export:", err)
			return true, 1
		}
		var cfg *TelegramConfig
		if withTG {
			cfg = &tg().cfg
		}
		var buf bytes.Buffer
		s.View(func(db *DB) { err = WriteBundle(&buf, db, "real", cfg, time.Now()) })
		if err == nil {
			if len(files) == 0 || files[0] == "-" {
				_, err = stdout.Write(buf.Bytes())
			} else {
				err = os.WriteFile(files[0], buf.Bytes(), 0o600)
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "export:", err)
			return true, 1
		}
		var sum BundleSummary
		s.View(func(db *DB) { sum = summarise(db) })
		fmt.Fprintln(stderr, "exported", sum)
		return true, 0
	case "import":
		if len(files) != 1 {
			usage()
			return true, 2
		}
		var in io.Reader = os.Stdin
		if files[0] != "-" {
			fh, err := os.Open(files[0])
			if err != nil {
				fmt.Fprintln(stderr, "import:", err)
				return true, 1
			}
			defer fh.Close()
			in = fh
		}
		db, b, err := ReadBundle(in)
		if err != nil {
			fmt.Fprintln(stderr, "import:", err)
			return true, 1
		}
		s, err := OpenStore(dataFile)
		if err != nil {
			// an unreadable current file is replaced too, after it is copied aside
			s = &Store{path: dataFile}
			s.db.init()
		}
		backup, err := s.Replace(db, time.Now())
		if err != nil {
			fmt.Fprintln(stderr, "import:", err)
			return true, 1
		}
		fmt.Fprintln(stderr, "imported", summarise(db))
		if backup != "" {
			fmt.Fprintln(stderr, "previous data kept as", backup)
		}
		if withTG {
			if b.Telegram == nil {
				fmt.Fprintln(stderr, "the export holds no Telegram settings")
			} else {
				t := tg()
				t.cfg = *b.Telegram
				if err := t.saveLocked(); err != nil {
					fmt.Fprintln(stderr, "import: telegram:", err)
					return true, 1
				}
				fmt.Fprintln(stderr, "Telegram settings restored")
			}
		}
		return true, 0
	case "help", "-h", "--help":
		usage()
		return true, 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n", cmd)
	usage()
	return true, 2
}
