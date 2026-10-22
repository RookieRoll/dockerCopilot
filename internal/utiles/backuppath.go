package utiles

import (
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/config"
)

func BackupDir() string { return config.LegacyBackupDirectory() }
func ResolveBackupPath(filename string, extensions ...string) (string, error) {
	return ResolveBackupPathIn(BackupDir(), filename, extensions...)
}
func ResolveBackupPathIn(directory, filename string, extensions ...string) (string, error) {
	return backupService.NewService(nil, directory).Resolve(filename, extensions...)
}
