package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreList_DefaultPatterns(t *testing.T) {
	// Create a temp directory
	tmpDir, err := os.MkdirTemp("", "contextshrinker-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	il, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to create ignore list: %v", err)
	}

	tests := []struct {
		path   string
		ignore bool
	}{
		{"main.go", false},
		{"src/utils.go", false},
		{"node_modules/react/index.js", true},
		{"vendor/github.com/foo/bar.go", true},
		{"src/vendor/foo.go", true}, // vendor segment is ignored
		{".git/config", true},
		{"build/bin/contextshrinker", true},
		{"target/debug/app", true},
		{"__pycache__/main.cpython-39.pyc", true},
		{".venv/lib/python3.9/site-packages/requests/api.py", true},
		{"venv/bin/pip", true},
		{".cache/go-build/somehash", true},
		{"dist/bundle.js", true},
		{"out/main.exe", true},
		{".vscode/settings.json", true},
		{"tmp/data.json", true},
		{"src/testdata/input.txt", true},
		{"test_cache/some-cache-file", true},
		{"test-cache/some-cache-file", true},
		{".test_cache/some-cache-file", true},
		{"main.test", true},
		{"coverage.out", true},
		{"coverage.coverprofile", true},
		{"profile.cov", true},
		{"go.work", true},
		{"go.work.sum", true},
	}

	for _, tt := range tests {
		got := il.ShouldIgnore(tt.path)
		if got != tt.ignore {
			t.Errorf("ShouldIgnore(%q) = %v; want %v", tt.path, got, tt.ignore)
		}
	}
}

func TestIgnoreList_CustomPatterns(t *testing.T) {
	// Create a temp directory
	tmpDir, err := os.MkdirTemp("", "contextshrinker-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Write custom .contextshrinkerignore
	content := `
# Comments are ignored

*.log
/tmp-output
temp_*
`
	err = os.WriteFile(filepath.Join(tmpDir, ".contextshrinkerignore"), []byte(content), 0644)
	if err != nil {
		t.Fatalf("failed to write .contextshrinkerignore: %v", err)
	}

	il, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to create ignore list: %v", err)
	}

	tests := []struct {
		path   string
		ignore bool
	}{
		{"main.go", false},
		{"app.log", true},
		{"logs/app.log", true}, // *.log matching inside folders
		{"tmp-output/file.txt", true},
		{"temp_data.json", true},
		{"src/temp_data.json", true},
	}

	for _, tt := range tests {
		got := il.ShouldIgnore(tt.path)
		if got != tt.ignore {
			t.Errorf("ShouldIgnore(%q) = %v; want %v", tt.path, got, tt.ignore)
		}
	}
}

func TestIgnoreList_DotContextShrinkerDirectory(t *testing.T) {
	// Create a temp directory
	tmpDir, err := os.MkdirTemp("", "contextshrinker-test-dotcontextshrinker")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Verify that loading ignore list does NOT automatically create .csignore or .contextshrinker
	il, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to create ignore list: %v", err)
	}

	csignorePath := filepath.Join(tmpDir, ".csignore")
	if _, errStat := os.Stat(csignorePath); !os.IsNotExist(errStat) {
		t.Error("expected .csignore NOT to be programmatically created, but it was")
	}

	legacyPath1 := filepath.Join(tmpDir, ".contextshrinker", "ignore")
	if _, errStat := os.Stat(legacyPath1); !os.IsNotExist(errStat) {
		t.Error("expected .contextshrinker/ignore NOT to be programmatically created, but it was")
	}

	// Verify default patterns include .contextshrinker and contextshrinker_graph.html
	if !il.ShouldIgnore(".contextshrinker/db") {
		t.Error("expected .contextshrinker/db to be ignored by default patterns")
	}
	if !il.ShouldIgnore("contextshrinker_graph.html") {
		t.Error("expected contextshrinker_graph.html to be ignored by default patterns")
	}

	// 1. Test .csignore loader
	customContent := "*.tmp\n"
	if errWrite := os.WriteFile(csignorePath, []byte(customContent), 0644); errWrite != nil {
		t.Fatalf("failed to write .csignore: %v", errWrite)
	}

	il2, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to reload ignore list: %v", err)
	}
	if !il2.ShouldIgnore("data.tmp") {
		t.Error("expected data.tmp to be ignored after writing custom pattern to .csignore")
	}

	// Remove .csignore and test legacy .contextshrinker/ignore fallback
	os.Remove(csignorePath)
	if errMkdir := os.MkdirAll(filepath.Dir(legacyPath1), 0755); errMkdir != nil {
		t.Fatalf("failed to create legacy dir: %v", errMkdir)
	}
	if errWrite := os.WriteFile(legacyPath1, []byte("*.legacy1\n"), 0644); errWrite != nil {
		t.Fatalf("failed to write legacy1: %v", errWrite)
	}

	il3, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to load legacy1: %v", err)
	}
	if !il3.ShouldIgnore("data.legacy1") {
		t.Error("expected data.legacy1 to be ignored via legacy fallback 1")
	}

	// Remove legacy1 and test legacy .contextshrinkerignore fallback
	os.Remove(legacyPath1)
	legacyPath2 := filepath.Join(tmpDir, ".contextshrinkerignore")
	if errWrite := os.WriteFile(legacyPath2, []byte("*.legacy2\n"), 0644); errWrite != nil {
		t.Fatalf("failed to write legacy2: %v", errWrite)
	}

	il4, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to load legacy2: %v", err)
	}
	if !il4.ShouldIgnore("data.legacy2") {
		t.Error("expected data.legacy2 to be ignored via legacy fallback 2")
	}
}
