package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	kuzu "github.com/kuzudb/go-kuzu"

	"github.com/bauhaus28/contextshrinker/internal/db"
	"github.com/bauhaus28/contextshrinker/internal/ignore"
	"github.com/bauhaus28/contextshrinker/internal/indexer"
	"github.com/bauhaus28/contextshrinker/internal/lsp"
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

func TestDatabaseReadOnlyFallback(t *testing.T) {
	if os.Getenv("BE_DB_RO_LOCKER") == "1" {
		dbPath := os.Getenv("DB_PATH")
		// Open the database in read-only mode directly using kuzu to hold a read-only lock
		config := kuzu.DefaultSystemConfig()
		config.ReadOnly = true
		db1, err := kuzu.OpenDatabase(dbPath, config)
		if err != nil {
			os.Exit(1)
		}
		defer db1.Close()
		// Sleep long enough for the test to run
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}

	tmpDir, err := os.MkdirTemp("", "contextshrinker-test-ro-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")

	// Initialize the database first (must be done in read-write mode)
	initDB, err := db.NewDatabase(dbPath)
	if err != nil {
		t.Fatalf("failed to initialize database: %v", err)
	}
	initDB.Close()

	// Start subprocess to hold the read-only lock
	cmd := exec.Command(os.Args[0], "-test.run=TestDatabaseReadOnlyFallback")
	cmd.Env = append(os.Environ(), "BE_DB_RO_LOCKER=1", "DB_PATH="+dbPath)
	err = cmd.Start()
	if err != nil {
		t.Fatalf("failed to start subprocess: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait for the subprocess to open the database and hold the read-only lock
	var db2 *db.Database
	var lastErr error
	for i := 0; i < 20; i++ {
		time.Sleep(200 * time.Millisecond)
		db2, lastErr = db.NewDatabase(dbPath)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		t.Fatalf("failed to open database: %v", lastErr)
	}
	defer db2.Close()

	if !db2.IsReadOnly() {
		t.Error("expected second database connection to fallback to read-only, but it was read-write")
	}
}

func TestDatabaseConcurrency(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "contextshrinker-test-concurrency-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")

	database, err := db.NewDatabase(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	// Setup initial data so reads actually query something
	_ = database.InsertFile("test.go", "hash1", 12345)
	_ = database.InsertFunction(db.FunctionEntity{
		ID:       "fn1",
		Name:     "MyFunc",
		FilePath: "test.go",
	})

	stopChan := make(chan struct{})
	var wg sync.WaitGroup

	// Goroutine 1: Continuous Writes
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stopChan:
				return
			default:
				_ = database.InsertFile(fmt.Sprintf("test_%d.go", i), "hash", int64(i))
				i++
			}
		}
	}()

	// Goroutine 2: Continuous Reads (IsEmpty)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopChan:
				return
			default:
				_, _ = database.IsEmpty()
			}
		}
	}()

	// Goroutine 3: Continuous Reads (SearchCodebase)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopChan:
				return
			default:
				_, _ = database.SearchCodebase("My")
			}
		}
	}()

	// Goroutine 4: Continuous Reads (GetCallChain)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopChan:
				return
			default:
				_, _ = database.GetCallChain("MyFunc", 2)
			}
		}
	}()

	// Run concurrently for 500 milliseconds
	time.Sleep(500 * time.Millisecond)
	close(stopChan)
	wg.Wait()
}

func TestInitCommand(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "contextshrinker-init-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Save old workspacePath
	oldWorkspacePath := workspacePath
	defer func() { workspacePath = oldWorkspacePath }()

	workspacePath = tmpDir

	// Run init
	runInit()

	// Assert files were created
	csignorePath := filepath.Join(tmpDir, ".csignore")
	if _, err := os.Stat(csignorePath); os.IsNotExist(err) {
		t.Error("expected .csignore to be created by init command")
	}

	schwobDir := filepath.Join(tmpDir, ".contextshrinker")
	if info, err := os.Stat(schwobDir); os.IsNotExist(err) || !info.IsDir() {
		t.Error("expected .contextshrinker directory to be created by init command")
	}

	// Verify content of .csignore
	content, err := os.ReadFile(csignorePath)
	if err != nil {
		t.Fatalf("failed to read .csignore: %v", err)
	}
	if !strings.Contains(string(content), ".vitepress") {
		t.Error("expected .csignore to contain .vitepress")
	}
}

func TestReindexForceClearsDatabase(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "contextshrinker-reindex-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	oldWorkspacePath := workspacePath
	oldReindexForce := reindexForce
	oldDbPath := dbPath
	defer func() {
		workspacePath = oldWorkspacePath
		reindexForce = oldReindexForce
		dbPath = oldDbPath
	}()

	workspacePath = tmpDir
	dbPath = filepath.Join(tmpDir, ".contextshrinker", "db")
	reindexForce = false

	// Initialize the DB first
	db1, err := getDBAndIngestIfNeeded(tmpDir)
	if err != nil {
		t.Fatalf("failed to initialize db first time: %v", err)
	}
	// Write a file node to verify it gets cleared
	err = db1.InsertFile("dummy.go", "hash", 12345)
	if err != nil {
		t.Fatalf("failed to write dummy node: %v", err)
	}
	db1.Close()

	// Now set reindexForce to true
	reindexForce = true
	db2, err := getDBAndIngestIfNeeded(tmpDir)
	if err != nil {
		t.Fatalf("failed to open db with reindex force: %v", err)
	}
	defer db2.Close()

	// Verify that the file node is gone since the database was recreated
	exists, err := db2.FileExists("dummy.go")
	if err != nil {
		t.Fatalf("failed to check file exists: %v", err)
	}
	if exists {
		t.Error("expected database to be cleared and dummy.go to be deleted, but it still exists")
	}
}

func TestClonesAndDeadCode(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "contextshrinker-test-clones-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")

	database, err := db.NewDatabase(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	// Insert files
	_ = database.InsertFile("file1.go", "hash1", 100)
	_ = database.InsertFile("file2.go", "hash2", 200)

	// Insert functions with identical AST hashes (clones)
	fn1 := db.FunctionEntity{
		ID:         "file1.go:10:0:helperA",
		Name:       "helperA",
		FilePath:   "file1.go",
		ASTHash:    "abcde12345",
		IsExported: false,
	}
	fn2 := db.FunctionEntity{
		ID:         "file2.go:20:0:helperB",
		Name:       "helperB",
		FilePath:   "file2.go",
		ASTHash:    "abcde12345", // same hash
		IsExported: false,
	}
	// Insert an unexported function with no calls (dead code)
	fn3 := db.FunctionEntity{
		ID:         "file1.go:30:0:unusedFunc",
		Name:       "unusedFunc",
		FilePath:   "file1.go",
		ASTHash:    "xyz789",
		IsExported: false,
	}
	// Insert an exported function (not considered dead code even if not called)
	fn4 := db.FunctionEntity{
		ID:         "file1.go:40:0:ExportedFunc",
		Name:       "ExportedFunc",
		FilePath:   "file1.go",
		ASTHash:    "qwerty",
		IsExported: true,
	}

	_ = database.InsertFunction(fn1)
	_ = database.InsertFunction(fn2)
	_ = database.InsertFunction(fn3)
	_ = database.InsertFunction(fn4)

	// Verify GetClones finds the clones
	clones, err := database.GetClones()
	if err != nil {
		t.Fatalf("failed to get clones: %v", err)
	}
	if len(clones) != 1 {
		t.Errorf("expected 1 clone group, got %d", len(clones))
	} else {
		group := clones[0]
		if group.ASTHash != "abcde12345" {
			t.Errorf("expected clone group hash 'abcde12345', got %q", group.ASTHash)
		}
		if len(group.Functions) != 2 {
			t.Errorf("expected 2 functions in clone group, got %d", len(group.Functions))
		}
	}

	// Verify GetDeadCode finds unusedFunc but not ExportedFunc
	dead, err := database.GetDeadCode()
	if err != nil {
		t.Fatalf("failed to get dead code: %v", err)
	}

	foundUnused := false
	foundExported := false
	for _, fn := range dead {
		if fn.FunctionName == "unusedFunc" {
			foundUnused = true
		}
		if fn.FunctionName == "ExportedFunc" {
			foundExported = true
		}
	}

	if !foundUnused {
		t.Error("expected unusedFunc to be flagged as dead code")
	}
	if foundExported {
		t.Error("ExportedFunc should not be flagged as dead code")
	}
}


