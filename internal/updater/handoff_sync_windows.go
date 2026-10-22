//go:build windows

package updater

// Windows does not expose the directory fsync behavior used on Unix. File
// contents themselves are flushed before the atomic hard-link publication.
func syncHandoffDirectory(string) error { return nil }
