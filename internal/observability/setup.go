package observability

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
)

var setupOnce sync.Once

type fanoutWriter struct {
	mu     sync.Mutex
	stdout io.Writer
	file   io.Writer
}

type managedWriter struct {
	delegate logx.Writer
	file     io.Closer
	once     sync.Once
	err      error
}

func (w *managedWriter) Alert(v any)                          { w.delegate.Alert(v) }
func (w *managedWriter) Debug(v any, fields ...logx.LogField) { w.delegate.Debug(v, fields...) }
func (w *managedWriter) Error(v any, fields ...logx.LogField) { w.delegate.Error(v, fields...) }
func (w *managedWriter) Info(v any, fields ...logx.LogField)  { w.delegate.Info(v, fields...) }
func (w *managedWriter) Severe(v any)                         { w.delegate.Severe(v) }
func (w *managedWriter) Slow(v any, fields ...logx.LogField)  { w.delegate.Slow(v, fields...) }
func (w *managedWriter) Stack(v any)                          { w.delegate.Stack(v) }
func (w *managedWriter) Stat(v any, fields ...logx.LogField)  { w.delegate.Stat(v, fields...) }
func (w *managedWriter) Close() error {
	w.once.Do(func() {
		_ = w.delegate.Close()
		w.err = w.file.Close()
	})
	return w.err
}

func (w *fanoutWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stdout != nil {
		_, _ = w.stdout.Write(p)
	}
	if w.file != nil {
		_, _ = w.file.Write(p)
	}
	return len(p), nil
}

func (w *fanoutWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.file.(io.Closer); ok {
		_ = c.Close()
	}
	w.file = nil
	return nil
}

// Setup installs the process-wide JSON log pipeline once. File errors are
// handled by the rotating writer and never prevent startup or stdout logging.
func Setup(directory, level string, retentionDays int, maxFileSize, maxTotalSize int64) error {
	setupOnce.Do(func() {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" {
			level = "info"
		}
		// SetUp establishes go-zero's JSON encoding and level policy. Replace its
		// default console writer to avoid duplicate output while preserving the
		// library's field processing and structured log serialization.
		_ = logx.SetUp(logx.LogConf{Mode: "console", Encoding: "json", Level: level, Stat: true})
		if previous := logx.Reset(); previous != nil {
			if closer, ok := previous.(io.Closer); ok {
				_ = closer.Close()
			}
		}
		file := NewRotatingWriter(Options{Directory: directory, RetentionDays: retentionDays, MaxFileSize: maxFileSize, MaxTotalSize: maxTotalSize})
		logx.SetWriter(&managedWriter{delegate: logx.NewWriter(&fanoutWriter{stdout: os.Stdout, file: file}), file: file})
	})
	return nil
}
