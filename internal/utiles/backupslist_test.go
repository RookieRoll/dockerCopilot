package utiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupListMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backup")
	t.Setenv("BACKUP_DIR", dir)
	list, err := BackupList(nil)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("missing directory: %v, %v", list, err)
	}
	dir = filepath.Join(t.TempDir(), "file")
	t.Setenv("BACKUP_DIR", dir)
	if err = os.WriteFile(dir, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = BackupList(nil); err == nil {
		t.Fatal("non-directory error suppressed")
	}
}
