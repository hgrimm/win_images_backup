// Image Backup CLI for Windows 10
// - Scans all local and connected drives for image files (configurable exclusions)
// - Deduplicates using SHA-256 hash
// - Copies to a target drive (external HDD) while preserving the original path structure
// - Maintains a simple JSON index of already backed-up hashes
// - Parallel processing and basic progress reporting
//
// Build (Windows):
//   go build -o win_images_backup.exe
//
// Example:
//   win_images_backup.exe -dest E:\backup -exclude D,F -exclude-dirs "Windows,Program Files,ProgramData" -workers 4 -log backup.log
//
// Notes:
// - The destination drive (dest) is automatically excluded from scanning.
// - You can exclude additional directories via -exclude-dirs (names or absolute paths).
//   Default excludes include Windows/, Program Files/, ProgramData/, recycle and temp folders; disable via -no-default-excludes.
// - LONG PATHS: The \\?\ prefix (or \\?\UNC for UNC paths) is added only when necessary (UNC or very long path).
// - The index file defaults to <dest>\backup_index.json.
// - Default image extensions are listed in defaultExts and can be overridden via -ext.

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Job describes a file to back up
type job struct {
	srcPath string
	rel     string
	drive   string
	size    int64
}

// Flags
var (
	destDir   = flag.String("dest", "", "Zielverzeichnis auf externer Festplatte, z. B. E:\\backup")
	exclude   = flag.String("exclude", "", "Kommagetrennte Liste auszuschließender Laufwerksbuchstaben, z. B. C,D")
	extFlag   = flag.String("ext", "", "Kommagetrennte Liste von Dateierweiterungen (override). Beispiel: jpg,jpeg,png,heic")
	workers   = flag.Int("workers", max(2, runtime.NumCPU()/2), "Anzahl paralleler Worker")
	indexName = flag.String("index", "backup_index.json", "Name/Relativpfad der Indexdatei im Ziel")
	dryRun    = flag.Bool("dry-run", false, "Nur anzeigen, nichts kopieren")
	logFile   = flag.String("log", "", "Optionaler Pfad für Log-Datei")

	excludeDirs       = flag.String("exclude-dirs", "", "Kommagetrennte Liste auszuschließender Verzeichnisse (Namen oder absolute Pfade)")
	noDefaultExcludes = flag.Bool("no-default-excludes", false, "Deaktiviert Standard-Verzeichnis-Ausschlüsse")
)

var idxMu sync.Mutex

var defaultExts = []string{
	".jpg", ".jpeg", ".png", ".gif", ".bmp", ".tiff", ".tif", ".webp",
	".heic", ".heif",
	".raw", ".cr2", ".nef", ".arw", ".rw2", ".orf", ".sr2", ".dng",
	".psd", ".svg",
}

// Default directory excludes (can be disabled via -no-default-excludes)
var defaultDirExcludes = []string{
	"Windows", "Program Files", "Program Files (x86)", "ProgramData",
	"$Recycle.Bin", "System Volume Information", "AppData", "Temp", "tmp",
}

type dirExcluder struct {
	names       map[string]bool
	absPrefixes []string
}

func newDirExcluder(flagVal string, useDefaults bool, destRoot string) dirExcluder {
	de := dirExcluder{names: make(map[string]bool)}
	if useDefaults {
		for _, n := range defaultDirExcludes {
			de.names[strings.ToLower(n)] = true
		}
	}
	if flagVal != "" {
		for _, raw := range strings.Split(flagVal, ",") {
			s := strings.TrimSpace(raw)
			if s == "" {
				continue
			}
			low := strings.ToLower(s)
			// Detect absolute paths (C:\..., \\server\share)
			if strings.Contains(low, ":\\") || strings.HasPrefix(low, `\\`) {
				de.absPrefixes = append(de.absPrefixes, strings.ToLower(filepath.Clean(s)))
			} else {
				de.names[low] = true
			}
		}
	}
	// Always exclude the destination root by absolute prefix
	if destRoot != "" {
		de.absPrefixes = append(de.absPrefixes, strings.ToLower(filepath.Clean(destRoot)))
	}
	return de
}

func (de dirExcluder) skipDir(path string, d os.DirEntry) bool {
	name := strings.ToLower(d.Name())
	if de.names[name] {
		return true
	}
	p := strings.ToLower(filepath.Clean(path))
	for _, pre := range de.absPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// Index keeps a set of hashes (and minor metadata)
type Index struct {
	Version int               `json:"version"`
	Hashes  map[string]uint32 `json:"hashes"` // value: copy count (optional)
	Updated time.Time         `json:"updated"`
}

func main() {
	flag.Parse()

	if *destDir == "" {
		fmt.Println("Fehler: -dest ist erforderlich (z. B. E:\\backup)")
		os.Exit(1)
	}

	// Logging setup
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Printf("Konnte Log-Datei nicht öffnen: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	// Normalize destination and ensure it exists
	absDest, err := filepath.Abs(*destDir)
	if err != nil {
		fatalf("Zielpfad ungültig: %v", err)
	}
	if !*dryRun {
		if err := os.MkdirAll(absDest, 0755); err != nil {
			fatalf("Konnte Zielverzeichnis nicht erstellen: %v", err)
		}
	}

	// Exclude destination drive automatically
	destDrive := driveLetter(absDest)
	excluded := parseExclude(*exclude)
	if destDrive != "" {
		excluded[strings.ToUpper(destDrive)] = true
	}

	// Prepare extension set
	exts := prepareExts(*extFlag)

	// Load/prepare index
	indexPath := *indexName
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(absDest, *indexName)
	}
	idx := loadIndex(indexPath)

	// Enumerate drives A:..Z:
	drives := listDrives()
	var sources []string
	for _, d := range drives {
		dl := strings.TrimSuffix(strings.ToUpper(d), ":\\")
		if excluded[dl] {
			log.Printf("Laufwerk %s ist ausgeschlossen\n", d)
			continue
		}
		sources = append(sources, d)
	}
	if len(sources) == 0 {
		fatalf("Keine Quell-Laufwerke gefunden (nach Ausschlüssen)")
	}

	fmt.Printf("Ziel: %s (Index: %s)\n", absDest, indexPath)
	fmt.Printf("Quellen: %s\n", strings.Join(sources, ", "))
	fmt.Printf("Worker: %d, Dry-Run: %v\n", *workers, *dryRun)

	// Directory excludes
	de := newDirExcluder(*excludeDirs, !*noDefaultExcludes, absDest)

	// Work queue & workers
	jobs := make(chan job, 1024)
	var wg sync.WaitGroup

	results := make(chan string, 1024)
	errs := make(chan error, 64)

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range jobs {
				if err := processFile(j, absDest, indexPath, idx, *dryRun, results); err != nil {
					errs <- fmt.Errorf("worker %d: %w", id, err)
				}
			}
		}(i + 1)
	}

	// Error logger
	go func() {
		for e := range errs {
			log.Printf("FEHLER: %v\n", e)
		}
	}()

	// Progress reporter
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var countScanned, countQueued, countCopied, countSkipped int64
		for {
			select {
			case <-ticker.C:
				fmt.Printf("Scanned: %d | Queued: %d | Copied: %d | Skipped: %d\r", countScanned, countQueued, countCopied, countSkipped)
			case r, ok := <-results:
				if !ok {
					fmt.Printf("\n")
					close(done)
					return
				}
				switch {
				case strings.HasPrefix(r, "SCANNED:"):
					countScanned++
				case strings.HasPrefix(r, "QUEUED:"):
					countQueued++
				case strings.HasPrefix(r, "COPIED:"):
					countCopied++
				case strings.HasPrefix(r, "SKIPPED:"):
					countSkipped++
				}
			}
		}
	}()

	// Walk all sources and enqueue files
	for _, root := range sources {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// Access errors etc.
				log.Printf("Zugriffsproblem bei %s: %v\n", path, err)
				return nil
			}
			results <- "SCANNED:1"
			if d.IsDir() {
				if de.skipDir(path, d) {
					return filepath.SkipDir
				}
				return nil
			}
			// Ignore symlinks
			if isSymlink(d) {
				return nil
			}
			// Extension filter
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if !exts[ext] {
				return nil
			}
			// File size
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = strings.TrimPrefix(path, root)
			}
			j := job{
				srcPath: path,
				rel:     rel,
				drive:   strings.TrimSuffix(strings.ToUpper(root), ":\\"),
				size:    info.Size(),
			}
			results <- "QUEUED:1"
			jobs <- j
			return nil
		})
		if err != nil {
			log.Printf("Fehler beim Durchlauf von %s: %v\n", root, err)
		}
	}

	close(jobs)
	wg.Wait()
	close(results)
	<-done

	// Final index save
	idx.Updated = time.Now().UTC()
	if err := saveIndex(indexPath, idx); err != nil {
		log.Printf("Index-Speicherfehler: %v\n", err)
	}

	fmt.Println("Fertig.")
}

func fatalf(f string, a ...any) {
	fmt.Printf(f+"\n", a...)
	os.Exit(1)
}

func parseExclude(s string) map[string]bool {
	m := make(map[string]bool)
	if s == "" {
		return m
	}
	for _, part := range strings.Split(s, ",") {
		p := strings.ToUpper(strings.TrimSpace(part))
		p = strings.TrimSuffix(p, ":")
		m[p] = true
	}
	return m
}

func prepareExts(flagVal string) map[string]bool {
	set := make(map[string]bool)
	if strings.TrimSpace(flagVal) == "" {
		for _, e := range defaultExts {
			set[strings.ToLower(e)] = true
		}
		return set
	}
	for _, e := range strings.Split(flagVal, ",") {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		set[e] = true
	}
	return set
}

func listDrives() []string {
	// Probe A:..Z: by stat
	var drives []string
	for c := 'A'; c <= 'Z'; c++ {
		root := fmt.Sprintf("%c:\\", c)
		if _, err := os.Stat(root); err == nil {
			drives = append(drives, root)
		}
	}
	sort.Strings(drives)
	return drives
}

func isSymlink(d os.DirEntry) bool {
	if d.Type()&os.ModeSymlink != 0 {
		return true
	}
	// Fallback: Lstat via Info
	if info, err := d.Info(); err == nil {
		return info.Mode()&os.ModeSymlink != 0
	}
	return false
}

func driveLetter(path string) string {
	if len(path) >= 2 && path[1] == ':' {
		return strings.ToUpper(string(path[0]))
	}
	return ""
}

// Decide conservatively whether the long-path prefix is needed
func needsLongPath(p string) bool {
	if strings.HasPrefix(p, `\\`) { // UNC
		return true
	}
	const maxShort = 248 // safely below MAX_PATH
	return len(p) > maxShort
}

func withLongPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	if strings.HasPrefix(p, `\\?\`) {
		return p
	}
	if strings.HasPrefix(p, `\\`) { // UNC
		return `\\?\UNC` + p[1:]
	}
	if needsLongPath(p) {
		return `\\?\` + p
	}
	return p
}

func processFile(j job, destRoot, indexPath string, idx *Index, dry bool, results chan<- string) error {
	// DRY-RUN: do not open/hash files, just show the plan
	if dry {
		dst := filepath.Join(destRoot, j.drive, j.rel)
		fmt.Printf("[DRY] Würde kopieren: %s -> %s\n", j.srcPath, dst)
		results <- "COPIED:1"
		return nil
	}

	// Compute hash
	h, err := hashFile(j.srcPath)
	if err != nil {
		// Skip invalid-name issues silently (common with installer artifacts)
		if isInvalidNameErr(err) {
			log.Printf("WARN: Überspringe ungültigen Pfad: %s (%v)\n", j.srcPath, err)
			results <- "SKIPPED:1"
			return nil
		}
		return fmt.Errorf("Hashfehlgeschlagen %s: %w", j.srcPath, err)
	}

	// Dedup check
	idxMu.Lock()
	_, exists := idx.Hashes[h]
	idxMu.Unlock()
	if exists {
		results <- "SKIPPED:1"
		return nil
	}

	// Destination path: <dest>/<DRIVE>/<rel>
	dst := filepath.Join(destRoot, j.drive, j.rel)

	// Ensure destination directory
	if err := os.MkdirAll(withLongPath(filepath.Dir(dst)), 0755); err != nil {
		return fmt.Errorf("Konnte Zielverzeichnis nicht erstellen: %w", err)
	}

	// If destination exists with same size, treat as identical
	if fi, err := os.Stat(withLongPath(dst)); err == nil {
		if fi.Size() == j.size {
			idxMu.Lock()
			idx.Hashes[h]++
			_ = saveIndex(indexPath, idx)
			idxMu.Unlock()
			results <- "SKIPPED:1"
			return nil
		}
	}

	if err := copyFile(j.srcPath, dst); err != nil {
		return fmt.Errorf("Kopiervorgang fehlgeschlagen: %w", err)
	}

	idxMu.Lock()
	idx.Hashes[h]++
	_ = saveIndex(indexPath, idx)
	idxMu.Unlock()

	log.Printf("Kopiert: %s -> %s\n", j.srcPath, dst)
	results <- "COPIED:1"
	return nil
}

func hashFile(path string) (string, error) {
	// Try normally first
	f, err := os.Open(path)
	if err != nil {
		if isInvalidNameErr(err) {
			return "", err
		}
		// Retry with long-path prefix
		f2, err2 := os.Open(withLongPath(path))
		if err2 != nil {
			return "", err
		}
		f = f2
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, 4*1024*1024) // 4 MiB
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := h.Write(buf[:n]); werr != nil {
				return "", werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	s, err := os.Open(withLongPath(src))
	if err != nil {
		if isInvalidNameErr(err) {
			return fmt.Errorf("Quelle ungültig: %w", err)
		}
		return err
	}
	defer s.Close()

	// Write to temp file then rename (atomic-ish)
	tmp := dst + ".part"
	if err := os.MkdirAll(withLongPath(filepath.Dir(dst)), 0755); err != nil {
		return err
	}
	d, err := os.OpenFile(withLongPath(tmp), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	bufw := bufio.NewWriterSize(d, 4*1024*1024)
	if _, err := io.Copy(bufw, s); err != nil {
		d.Close()
		return err
	}
	if err := bufw.Flush(); err != nil {
		d.Close()
		return err
	}
	if err := d.Close(); err != nil {
		return err
	}

	// Rename into place
	if err := os.Rename(withLongPath(tmp), withLongPath(dst)); err != nil {
		return err
	}
	return nil
}

func loadIndex(path string) *Index {
	idx := &Index{Version: 1, Hashes: make(map[string]uint32), Updated: time.Now().UTC()}
	b, err := os.ReadFile(withLongPath(path))
	if err != nil {
		return idx
	}
	if err := json.Unmarshal(b, idx); err != nil {
		log.Printf("Index konnte nicht gelesen werden, erstelle neu: %v\n", err)
		return &Index{Version: 1, Hashes: make(map[string]uint32), Updated: time.Now().UTC()}
	}
	return idx
}

func saveIndex(path string, idx *Index) error {
	idx.Updated = time.Now().UTC()
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(withLongPath(tmp), data, 0644); err != nil {
		return err
	}
	return os.Rename(withLongPath(tmp), withLongPath(path))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Detect ERROR_INVALID_NAME via message text (robust enough across locales)
func isInvalidNameErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid name") || strings.Contains(s, "ungültig")
}
