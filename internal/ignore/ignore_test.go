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

	// Verify that loading ignore list automatically creates .contextshrinker/ignore
	il, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to create ignore list: %v", err)
	}

	// Verify file was created
	createdIgnorePath := filepath.Join(tmpDir, ".contextshrinker", "ignore")
	if _, errStat := os.Stat(createdIgnorePath); os.IsNotExist(errStat) {
		t.Error("expected .contextshrinker/ignore to be programmatically created, but it was not")
	}

	// Verify default patterns include .contextshrinker and contextshrinker_graph.html
	if !il.ShouldIgnore(".contextshrinker/db") {
		t.Error("expected .contextshrinker/db to be ignored by default patterns")
	}
	if !il.ShouldIgnore("contextshrinker_graph.html") {
		t.Error("expected contextshrinker_graph.html to be ignored by default patterns")
	}

	// Now append a custom pattern to the newly created ignore file and reload
	customContent := `
# Added custom pattern
*.tmp
`
	if errWrite := os.WriteFile(createdIgnorePath, []byte(customContent), 0644); errWrite != nil {
		t.Fatalf("failed to overwrite ignore file: %v", errWrite)
	}

	il2, err := NewIgnoreList(tmpDir)
	if err != nil {
		t.Fatalf("failed to reload ignore list: %v", err)
	}

	if !il2.ShouldIgnore("data.tmp") {
		t.Error("expected data.tmp to be ignored after writing custom pattern to .contextshrinker/ignore")
	}
}
