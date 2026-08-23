package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileTailBoundsRecentLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	content := strings.Repeat("old\n", 100) + "final-result\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, err := readFileTail(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) > 32 || !strings.HasSuffix(tail, "final-result\n") || strings.HasPrefix(tail, "old\nold\nold\n") {
		t.Fatalf("tail = %q", tail)
	}
}
