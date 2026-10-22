package observability

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const logPrefix = "dockercopilot-"

type Options struct {
	Directory     string
	RetentionDays int
	MaxFileSize   int64
	MaxTotalSize  int64
	Now           func() time.Time
	Stderr        io.Writer
}

// RotatingWriter writes newline-delimited log records to daily, size-limited
// files. It includes the active file in the configured total-size budget.
type RotatingWriter struct {
	mu          sync.Mutex
	opts        Options
	file        *os.File
	date        string
	sequence    int
	size        int64
	lastWarning time.Time
	closed      bool
}

func NewRotatingWriter(opts Options) *RotatingWriter {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.RetentionDays <= 0 {
		opts.RetentionDays = 7
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = 20 << 20
	}
	if opts.MaxTotalSize <= 0 {
		opts.MaxTotalSize = 200 << 20
	}
	return &RotatingWriter{opts: opts}
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	now := w.opts.Now()
	if err := w.prepare(); err != nil {
		w.warn(now, err)
		return len(p), nil
	}
	if int64(len(p)) > w.opts.MaxFileSize {
		w.warn(now, fmt.Errorf("log record (%d bytes) exceeds per-file limit", len(p)))
		return len(p), nil
	}
	if w.file != nil && (w.date != now.Format("2006-01-02") || w.size+int64(len(p)) > w.opts.MaxFileSize) {
		if err := w.rotate(); err != nil {
			w.disable(now, err)
			return len(p), nil
		}
	}
	if w.file == nil {
		if err := w.open(now, int64(len(p))); err != nil {
			w.warn(now, err)
			return len(p), nil
		}
	}
	if err := w.cleanup(now, int64(len(p))); err != nil {
		w.disable(now, err)
		return len(p), nil
	}
	n, err := w.file.Write(p)
	if err != nil {
		w.disable(now, err)
		return len(p), nil
	}
	w.size += int64(n)
	if n != len(p) {
		w.disable(now, io.ErrShortWrite)
	}
	return len(p), nil
}

func (w *RotatingWriter) prepare() error {
	if err := os.MkdirAll(w.opts.Directory, 0755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	return nil
}

func (w *RotatingWriter) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			w.file = nil
			return err
		}
		w.file = nil
	}
	w.sequence++
	return nil
}

func (w *RotatingWriter) open(now time.Time, incoming int64) error {
	date := now.Format("2006-01-02")
	if date != w.date {
		w.date, w.sequence = date, 0
	}
	for {
		name := w.filename()
		f, err := os.OpenFile(filepath.Join(w.opts.Directory, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("stat log file: %w", err)
		}
		if info.Size()+incoming <= w.opts.MaxFileSize {
			w.file, w.size = f, info.Size()
			return nil
		}
		_ = f.Close()
		w.sequence++
	}
}

func (w *RotatingWriter) filename() string {
	if w.sequence == 0 {
		return logPrefix + w.date + ".jsonl"
	}
	return fmt.Sprintf("%s%s.%d.jsonl", logPrefix, w.date, w.sequence)
}

type logFile struct {
	name string
	mod  time.Time
	size int64
}

func isManagedLogFile(name string) bool {
	if !strings.HasPrefix(name, logPrefix) || !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, logPrefix), ".jsonl")
	date, sequence, hasSequence := strings.Cut(stem, ".")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return false
	}
	if !hasSequence {
		return true
	}
	if strings.Contains(sequence, ".") || sequence == "" {
		return false
	}
	for _, digit := range sequence {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return sequence != "0"
}

func (w *RotatingWriter) cleanup(now time.Time, incoming int64) error {
	entries, err := os.ReadDir(w.opts.Directory)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	var files []logFile
	var total int64
	cutoff := now.Add(-time.Duration(w.opts.RetentionDays) * 24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || !isManagedLogFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat log file: %w", err)
		}
		if info.ModTime().Before(cutoff) && (w.file == nil || filepath.Base(w.file.Name()) != entry.Name()) {
			if err := os.Remove(filepath.Join(w.opts.Directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove expired log: %w", err)
			}
			continue
		}
		files = append(files, logFile{entry.Name(), info.ModTime(), info.Size()})
		total += info.Size()
	}
	if total+incoming <= w.opts.MaxTotalSize {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, file := range files {
		if w.file != nil && filepath.Base(w.file.Name()) == file.name {
			continue
		}
		if err := os.Remove(filepath.Join(w.opts.Directory, file.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove over-capacity log: %w", err)
		}
		total -= file.size
		if total+incoming <= w.opts.MaxTotalSize {
			return nil
		}
	}
	if total+incoming > w.opts.MaxTotalSize {
		return fmt.Errorf("log directory exceeds total limit (%d bytes)", w.opts.MaxTotalSize)
	}
	return nil
}

func (w *RotatingWriter) disable(now time.Time, err error) {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	w.warn(now, err)
}

func (w *RotatingWriter) warn(now time.Time, err error) {
	if !w.lastWarning.IsZero() && now.Sub(w.lastWarning) < time.Minute {
		return
	}
	w.lastWarning = now
	_, _ = fmt.Fprintf(w.opts.Stderr, "dockerCopilot: file logging unavailable: %v\n", err)
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
