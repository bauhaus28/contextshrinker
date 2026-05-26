package main

import (
	"os"
	"path/filepath"
	"testing"

	"contextshrinker/internal/db"
	"contextshrinker/internal/ignore"
	"contextshrinker/internal/indexer"
	"contextshrinker/internal/lsp"
)

func TestIntegration_ContextShrinkerIndexer(t *testing.T) {
	// 1. Setup temporary workspace inside the project
	absWorkspace, err := filepath.Abs(filepath.Join(".", "test_temp_workspace"))
	if err != nil {
		t.Fatalf("failed to resolve absolute path: %v", err)
	}
	tmpWorkspace := absWorkspace
	if err := os.MkdirAll(tmpWorkspace, 0755); err != nil {
		t.Fatalf("failed to create temp workspace: %v", err)
	}
	defer os.RemoveAll(tmpWorkspace)

	goCode := `package main
// MyFunc is a test function.
func MyFunc() {
	println("Hello")
}

type MyStruct struct {
	Field string
}
`
	goFilePath := filepath.Join(tmpWorkspace, "main.go")
	if err := os.WriteFile(goFilePath, []byte(goCode), 0644); err != nil {
		t.Fatalf("failed to write go file: %v", err)
	}

	pyCode := `def my_py_func():
    """Python function doc"""
    pass
`
	pyFilePath := filepath.Join(tmpWorkspace, "app.py")
	if err := os.WriteFile(pyFilePath, []byte(pyCode), 0644); err != nil {
		t.Fatalf("failed to write py file: %v", err)
	}

	// 2. Setup Kuzu database
	absDbPath, _ := filepath.Abs("test_db_integration")
	dbPath := absDbPath
	database, err := db.NewDatabase(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() {
		database.Close()
		os.RemoveAll(dbPath)
	}()

	ignoreList, err := ignore.NewIgnoreList(tmpWorkspace)
	if err != nil {
		t.Fatalf("failed to load ignore rules: %v", err)
	}

	lspManager := lsp.NewLSPManager(tmpWorkspace)
	defer lspManager.Close()

	idx := indexer.NewIndexer(tmpWorkspace, database, ignoreList, lspManager)

	// 3. Run Pass 1 on the workspace
	if err := idx.IngestWorkspace(); err != nil {
		t.Fatalf("ingest workspace failed: %v", err)
	}

	// 4. Verify DB entities via search codebase
	results, err := database.SearchCodebase("My")
	if err != nil {
		t.Fatalf("search codebase failed: %v", err)
	}

	foundFunc := false
	foundStruct := false
	for _, res := range results {
		if res.Name == "MyFunc" && res.Type == "Function" {
			foundFunc = true
		}
		if res.Name == "MyStruct" && res.Type == "Class" {
			foundStruct = true
		}
	}

	if !foundFunc {
		t.Error("expected to find MyFunc in database search")
	}
	if !foundStruct {
		t.Error("expected to find MyStruct in database search")
	}

	// 5. Verify Python function
	pyResults, err := database.SearchCodebase("my_py")
	if err != nil {
		t.Fatalf("search py codebase failed: %v", err)
	}
	if len(pyResults) == 0 || pyResults[0].Name != "my_py_func" {
		t.Errorf("expected to find my_py_func, got: %+v", pyResults)
	}

	// 6. Verify File structure
	structItems, err := database.GetFileStructure(goFilePath)
	if err != nil {
		t.Fatalf("failed to get file structure: %v", err)
	}

	hasGoFunc := false
	hasGoStruct := false
	for _, item := range structItems {
		if item.Name == "MyFunc" && item.Type == "Function" {
			hasGoFunc = true
		}
		if item.Name == "MyStruct" && item.Type == "Class" {
			hasGoStruct = true
		}
	}

	if !hasGoFunc {
		t.Error("file structure is missing function MyFunc")
	}
	if !hasGoStruct {
		t.Error("file structure is missing class MyStruct")
	}
}
