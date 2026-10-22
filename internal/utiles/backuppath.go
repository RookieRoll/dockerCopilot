package utiles

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/onlyLTY/dockerCopilot/internal/config"
)

var backupFilenamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func BackupDir() string {
	return config.LegacyBackupDirectory()
}

func ResolveBackupPath(filename string, allowedExts ...string) (string, error) {
	return ResolveBackupPathIn(BackupDir(), filename, allowedExts...)
}

func ResolveBackupPathIn(backupDir, filename string, allowedExts ...string) (string, error) {
	name := strings.TrimSpace(filename)
	if name == "" {
		return "", fmt.Errorf("非法文件名")
	}
	if name != filepath.Base(name) {
		return "", fmt.Errorf("非法文件名")
	}
	if strings.ContainsAny(name, `/\`) || !backupFilenamePattern.MatchString(name) {
		return "", fmt.Errorf("非法文件名")
	}

	if len(allowedExts) > 0 {
		ext := strings.ToLower(filepath.Ext(name))
		allowed := false
		for _, candidate := range allowedExts {
			if ext == strings.ToLower(candidate) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("非法文件名")
		}
	}

	base := filepath.Clean(backupDir)
	fullPath := filepath.Clean(filepath.Join(base, name))
	basePrefix := base + string(os.PathSeparator)
	if fullPath != base && !strings.HasPrefix(fullPath, basePrefix) {
		return "", fmt.Errorf("非法文件名")
	}

	return fullPath, nil
}
