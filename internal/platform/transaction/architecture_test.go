package transaction_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBusinessSeamsCannotUseGORMTransactionBridge(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate transaction architecture test")
	}
	internalRoot := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return walkErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		source := string(content)
		rel, relErr := filepath.Rel(internalRoot, path)
		if relErr != nil {
			return relErr
		}

		if (strings.Contains(source, "transaction.GORM(") || strings.Contains(source, "transaction.WithinGORM(")) &&
			!strings.HasSuffix(path, "_gorm.go") && !strings.HasPrefix(rel, filepath.Join("platform", "transaction")) {
			t.Errorf("%s uses the GORM transaction bridge outside an adapter", rel)
		}
		if strings.Contains(source, `"gorm.io/gorm"`) &&
			(strings.Contains(rel, string(filepath.Separator)+"service"+string(filepath.Separator)) ||
				strings.Contains(rel, string(filepath.Separator)+"repository"+string(filepath.Separator)) && !strings.HasSuffix(path, "_gorm.go")) {
			t.Errorf("%s exposes GORM through a business seam", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
