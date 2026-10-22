package utiles

import (
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"os"
	"path/filepath"
)

func BackupList(ctx *svc.ServiceContext) ([]string, error) {
	dir := BackupDir()
	if ctx != nil {
		dir = ctx.Config.BackupDirectory()
	}
	return BackupListIn(dir)
}

func BackupListIn(dir string) ([]string, error) {
	var backupList []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			backupList = append(backupList, entry.Name())
		} else if !entry.IsDir() && filepath.Ext(entry.Name()) == ".yaml" {
			backupList = append(backupList, entry.Name())
		}
	}

	return backupList, nil
}
