package cli

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/paranoidi/fileindex/config"
	"github.com/paranoidi/fileindex/db"
)

func exportCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "export OUTPUT",
		Short: "Export files and instances to CSV",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := db.Resolve(args[0])
			if err != nil {
				return err
			}
			d, err := a.openDB()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Exporting database to: %s\n", path)
			start := time.Now()
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			defer f.Close()
			w := csv.NewWriter(f)
			w.UseCRLF = true
			w.Write(db.ExportHeader)
			md5s := map[string]bool{}
			n := 0
			if err := d.ExportRows(func(rec []string) error {
				md5s[rec[0]] = true
				n++
				return w.Write(rec)
			}); err != nil {
				return err
			}
			w.Flush()
			if err := w.Error(); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Export completed in %.2f seconds\nFiles exported: %d\nInstances exported: %d\nCSV file: %s\n",
				time.Since(start).Seconds(), len(md5s), n, path)
			return nil
		},
	}
}

// sha256 and path are required too, so a CSV the DB can't store fails
// validation instead of failing per batch.
var requiredColumns = []string{"md5", "sha256", "filesize", "file_created_at", "file_last_seen", "path", "filename",
	"mtime", "instance_created_at", "instance_last_seen"}

const importBatchSize = 1000

// readImportCSV reads the whole file and validates every row. It returns the
// valid rows and the row errors (at most 1000, then a "too many" line).
// ponytail: the CSV is held in memory; stream it if exports get huge.
func readImportCSV(path string) (rows []db.ImportRow, errs []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("Error reading CSV file: %v", err)
	}
	if len(records) == 0 {
		return nil, nil, fmt.Errorf("Error reading CSV file: empty file")
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[h] = i
	}
	var missing []string
	for _, c := range requiredColumns {
		if _, ok := col[c]; !ok {
			missing = append(missing, "'"+c+"'")
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, nil, fmt.Errorf("Error reading CSV file: CSV missing required columns: {%s}", strings.Join(missing, ", "))
	}

	for i, rec := range records[1:] {
		get := func(name string) string {
			if j, ok := col[name]; ok && j < len(rec) {
				return rec[j]
			}
			return ""
		}
		n := i + 2
		row := db.ImportRow{
			MD5: get("md5"), SHA256: get("sha256"), NoExifJPEG: get("sha256_noexif_jpeg"), SHA256_8KB: get("sha256_8kb"),
			FileCreated: get("file_created_at"), FileLastSeen: get("file_last_seen"),
			Path: get("path"), Filename: get("filename"), Mtime: get("mtime"),
			InstCreated: get("instance_created_at"), InstLastSeen: get("instance_last_seen"),
		}
		size, perr := config.ParseInt(get("filesize"))
		row.Size = int64(size)
		var e string
		switch {
		case row.MD5 == "":
			e = "Missing required md5 hash"
		case row.Filename == "":
			e = "Missing filename"
		case perr != nil:
			e = "Invalid filesize format: " + get("filesize")
		case size < 0:
			e = "Invalid filesize: " + get("filesize")
		case utf8.RuneCountInString(row.MD5) != 32:
			e = fmt.Sprintf("Invalid MD5 hash length: %d", utf8.RuneCountInString(row.MD5))
		case row.SHA256 == "":
			e = "Missing required sha256 hash"
		case utf8.RuneCountInString(row.SHA256) != 64: // diff prints the first 16
			e = fmt.Sprintf("Invalid SHA256 hash length: %d", utf8.RuneCountInString(row.SHA256))
		case row.Path == "" || row.Path == ".":
			e = "Missing path"
		}
		if e == "" {
			rows = append(rows, row)
			continue
		}
		errs = append(errs, fmt.Sprintf("Row %d: %s", n, e))
		if len(errs) > 1000 {
			errs = append(errs, "Too many validation errors, stopping validation")
			break
		}
	}
	return rows, errs, nil
}

func importCmd(a *app) *cobra.Command {
	var validateOnly bool
	cmd := &cobra.Command{
		Use:   "import INPUT",
		Short: "Import files and instances from CSV",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := db.Resolve(args[0])
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("CSV file not found: %s", path)
			}
			w := a.stdout
			if validateOnly {
				fmt.Fprintf(w, "Validating CSV file: %s\n", path)
			} else {
				fmt.Fprintf(w, "Importing from CSV file: %s\n", path)
			}
			start := time.Now()
			rows, errs, err := readImportCSV(path)
			if err != nil {
				return err
			}
			if len(errs) > 0 {
				if validateOnly {
					fmt.Fprintln(w, "✘ CSV file has validation errors:")
				} else {
					fmt.Fprintln(w, "✘ Import failed:")
				}
				for _, e := range errs {
					fmt.Fprintf(w, "  %s\n", e)
				}
				return exitCode(1)
			}
			md5s := map[string]bool{}
			for _, r := range rows {
				md5s[r.MD5] = true
			}
			if validateOnly {
				fmt.Fprintf(w, "✔ CSV file is valid\nFiles to import: %d\nInstances to import: %d\n", len(md5s), len(rows))
				return nil
			}

			d, err := a.openDB()
			if err != nil {
				return err
			}
			var files, instances int
			var batchErrs []string
			for chunk := range slices.Chunk(rows, importBatchSize) {
				f, i, err := d.ImportBatch(chunk)
				if err != nil {
					batchErrs = append(batchErrs, fmt.Sprintf("Database error during batch import: %v", err))
					continue
				}
				files, instances = files+f, instances+i
			}
			fmt.Fprintf(w, "Import completed in %.2f seconds\nFiles imported: %d\nInstances imported: %d\nTotal files processed: %d\nTotal instances processed: %d\n",
				time.Since(start).Seconds(), files, instances, len(md5s), len(rows))
			if len(batchErrs) > 0 { // a failed batch fails the command
				fmt.Fprintln(a.stderr, "\nErrors during import:")
				for _, e := range batchErrs {
					fmt.Fprintf(a.stderr, "  %s\n", e)
				}
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&validateOnly, "validate-only", false, "Only validate the CSV file without importing")
	return cmd
}
