package utiles

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writeTestTarGz(t *testing.T, path string, names []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, name := range names {
		content := "x"
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecompressTarGzPathSafety(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	// 正常条目可以解出(含嵌套路径,与官方发布包 dist/linux/<arch>/ 布局一致)
	good := filepath.Join(dir, "good.tar.gz")
	writeTestTarGz(t, good, []string{"app.txt", "dist/linux/amd64/dockerCopilot-new"})
	if err := decompressTarGz(good, dest); err != nil {
		t.Fatalf("正常归档解压失败: %v", err)
	}
	for _, name := range []string{"app.txt", "dist/linux/amd64/dockerCopilot-new"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Fatalf("正常条目未解出 %s: %v", name, err)
		}
	}

	// 穿越条目必须被拒绝且不落盘
	evil := filepath.Join(dir, "evil.tar.gz")
	writeTestTarGz(t, evil, []string{"../evil.txt"})
	if err := decompressTarGz(evil, dest); err == nil {
		t.Fatal("路径穿越条目未被拒绝")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
		t.Fatal("穿越文件被写出")
	}
}
