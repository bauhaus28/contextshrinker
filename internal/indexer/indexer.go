package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bauhaus28/contextshrinker/internal/db"
	"github.com/bauhaus28/contextshrinker/internal/ignore"
	"github.com/bauhaus28/contextshrinker/internal/lsp"
	"github.com/bauhaus28/contextshrinker/internal/parser"
)

type Indexer struct {
	WorkspaceRoot   string
	Database        *db.Database
	IgnoreList      *ignore.IgnoreList
	LspManager      *lsp.LSPManager
	FunctionsByFile map[string][]db.FunctionEntity
	FunctionsMu     sync.RWMutex
	logMu           sync.Mutex
	workspaceFiles  []string
}

func NewIndexer(workspaceRoot string, database *db.Database, ignoreList *ignore.IgnoreList, lspManager *lsp.LSPManager) *Indexer {
	return &Indexer{
		WorkspaceRoot:   workspaceRoot,
		Database:        database,
		IgnoreList:      ignoreList,
		LspManager:      lspManager,
		FunctionsByFile: make(map[string][]db.FunctionEntity),
	}
}

func (idx *Indexer) logProgress(format string, args ...any) {
	idx.logMu.Lock()
	defer idx.logMu.Unlock()
	fmt.Fprintf(os.Stderr, format, args...)
}

func (idx *Indexer) logWarning(format string, args ...any) {
	idx.logMu.Lock()
	defer idx.logMu.Unlock()
	fmt.Fprintf(os.Stderr, "\r                                                                                \r")
	log.Printf(format, args...)
}

// IngestWorkspace walks the workspace and performs Pass 1 and Pass 2 on all source files.
func (idx *Indexer) IngestWorkspace() error {
	var files []string
	err := filepath.WalkDir(idx.WorkspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(idx.WorkspaceRoot, path)
		if err != nil {
			return nil
		}

		if rel != "." && idx.IgnoreList.ShouldIgnore(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".go" || ext == ".py" || ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx" || ext == ".java" {
				files = append(files, path)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	idx.workspaceFiles = files

	// Pass 1: Tree-sitter scan
	log.Printf("Pass 1: Syntax parsing %d files...", len(files))
	for i, file := range files {
		idx.logProgress("\rPass 1: [%d/%d] Ingesting %s...                      ", i+1, len(files), filepath.Base(file))
		if err := idx.runPass1(file); err != nil {
			idx.logWarning("Pass 1 failed for %s: %v", file, err)
		}
	}
	idx.logProgress("\n")

	// Pass 2: LSP semantic indexing
	log.Printf("Pass 2: Building relational call graph for %d files...", len(files))

	numWorkers := 8
	if numWorkers > len(files) {
		numWorkers = len(files)
	}

	var wg sync.WaitGroup
	fileChan := make(chan string, len(files))
	for _, file := range files {
		fileChan <- file
	}
	close(fileChan)

	var completed int64
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range fileChan {
				idx.runPass2(file)
				curr := atomic.AddInt64(&completed, 1)
				idx.logProgress("\rPass 2: [%d/%d] Completed references query for %s                      ", curr, len(files), filepath.Base(file))
			}
		}()
	}
	wg.Wait()
	idx.logProgress("\n")

	return nil
}

func (idx *Indexer) runPass1(filePath string) error {
	// Calculate Hash & Last Modified
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	hash, err := calculateFileHash(filePath)
	if err != nil {
		return err
	}

	// Delete old references for the file first if it already exists (idempotent delta)
	exists, err := idx.Database.FileExists(filePath)
	if err == nil && exists {
		if err := idx.Database.DeleteFileEntities(filePath); err != nil {
			return err
		}
	}

	// Insert File Node
	if err := idx.Database.InsertFile(filePath, hash, info.ModTime().Unix()); err != nil {
		return err
	}

	// Run Tree-sitter Parse
	res, err := parser.ParseFile(filePath)
	if err != nil {
		return err
	}

	// Insert Functions
	idx.FunctionsMu.Lock()
	for _, fn := range res.Functions {
		if err := idx.Database.InsertFunction(fn); err != nil {
			log.Printf("Failed to insert function %s: %v", fn.Name, err)
			continue
		}
		idx.FunctionsByFile[filePath] = append(idx.FunctionsByFile[filePath], fn)
	}
	idx.FunctionsMu.Unlock()

	// Insert Classes
	for _, c := range res.Classes {
		if err := idx.Database.InsertClass(c); err != nil {
			log.Printf("Failed to insert class %s: %v", c.Name, err)
		}
	}

	// Insert Variables
	for _, v := range res.Variables {
		if err := idx.Database.InsertVariable(v); err != nil {
			log.Printf("Failed to insert variable %s: %v", v.Name, err)
		}
	}

	// Create CONTAINS Edges
	for _, rel := range res.Contains {
		switch rel.ParentType {
		case "File":
			_ = idx.Database.CreateContainsFileToEntity(rel.ParentID, rel.ChildID, rel.ChildType)
		case "Class":
			_ = idx.Database.CreateContainsClassToEntity(rel.ParentID, rel.ChildID, rel.ChildType)
		}
	}

	// Create IMPLEMENTS Edges
	for _, rel := range res.Implements {
		toClassID := idx.findClassIDByName(rel.ToClassName)
		if toClassID != "" {
			_ = idx.Database.CreateImplements(rel.FromClassID, toClassID)
		}
	}

	// Extract and record IMPORTS
	idx.extractAndRecordImports(res.Imports)

	return nil
}

func (idx *Indexer) runPass2(filePath string) {
	idx.FunctionsMu.RLock()
	fileFunctions := idx.FunctionsByFile[filePath]
	idx.FunctionsMu.RUnlock()

	if len(fileFunctions) == 0 {
		return
	}

	// Get LSP Client
	lang := detectLanguage(filePath)
	client, err := idx.LspManager.GetClient(lang)
	if err != nil {
		idx.logWarning("Skipping Pass 2 for %s: LSP not available (%v)", filePath, err)
		return
	}

	// Read file content
	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		idx.logWarning("Error: failed to read file content for Pass 2: %s: %v", filePath, err)
		return
	}
	contentStr := string(contentBytes)

	// Open file in LSP
	_ = client.DidOpen(filePath, contentStr)

	// Query references for each function
	edgesCreated := 0
	for _, fn := range fileFunctions {
		line, char := findNamePositionInFile(contentStr, fn.StartLine, fn.Name)
		locs, err := client.References(context.Background(), filePath, line, char)
		if err != nil {
			idx.logWarning("Warning: references query failed for %s:%s at line %d: %v", filepath.Base(filePath), fn.Name, line+1, err)
			continue
		}

		for _, loc := range locs {
			refPath, err := lsp.URIToPath(loc.URI)
			if err != nil {
				continue
			}

			// Find caller function that contains the reference line
			refLine := loc.Range.Start.Line + 1 // 1-indexed
			callerFnID := idx.findFunctionAtLine(refPath, refLine)
			if callerFnID != "" && callerFnID != fn.ID {
				if err := idx.Database.CreateCalls(callerFnID, fn.ID); err == nil {
					edgesCreated++
				}
			}
		}
	}
}

// HandleFileUpdate processes a file modification delta update.
func (idx *Indexer) HandleFileUpdate(filePath string) {
	idx.FunctionsMu.Lock()
	delete(idx.FunctionsByFile, filePath)
	idx.FunctionsMu.Unlock()

	// Run Pass 1
	if err := idx.runPass1(filePath); err != nil {
		log.Printf("Delta Pass 1 failed for %s: %v", filePath, err)
		return
	}

	// Run Pass 2
	idx.runPass2(filePath)
}

func (idx *Indexer) findClassIDByName(name string) string {
	res, err := idx.Database.FindClassIDByName(name)
	if err != nil {
		return ""
	}
	return res
}

func (idx *Indexer) extractAndRecordImports(imports []parser.ImportDecl) {
	for _, imp := range imports {
		targets := idx.resolveImportTarget(imp.SourcePath, imp.Path)
		for _, target := range targets {
			if target != imp.SourcePath {
				_ = idx.Database.CreateImportsFileToFile(imp.SourcePath, target)
			}
		}
	}
}

func (idx *Indexer) resolveImportTarget(sourceFile, importPath string) []string {
	importPath = filepath.ToSlash(importPath)
	var targets []string

	// 1. Relative imports
	if strings.HasPrefix(importPath, ".") {
		absDir := filepath.Join(filepath.Dir(sourceFile), importPath)
		extensions := []string{".go", ".py", ".js", ".jsx", ".ts", ".tsx", ".java"}
		for _, ext := range extensions {
			testPath := absDir + ext
			if _, err := os.Stat(testPath); err == nil {
				targets = append(targets, testPath)
				return targets
			}
		}
		if info, err := os.Stat(absDir); err == nil && info.IsDir() {
			for _, f := range idx.workspaceFiles {
				if strings.HasPrefix(f, absDir) {
					targets = append(targets, f)
				}
			}
			return targets
		}
	}

	// 2. Absolute/Package imports
	segments := strings.FieldsFunc(importPath, func(r rune) bool {
		return r == '.' || r == '/'
	})
	if len(segments) == 0 {
		return nil
	}

	for _, f := range idx.workspaceFiles {
		rel, err := filepath.Rel(idx.WorkspaceRoot, f)
		if err == nil {
			if matchImportToWorkspaceFile(rel, segments) {
				targets = append(targets, f)
			}
		}
	}

	return targets
}

func matchImportToWorkspaceFile(relPath string, segments []string) bool {
	relPath = filepath.ToSlash(relPath)
	fileSegs := strings.Split(relPath, "/")
	if len(fileSegs) == 0 {
		return false
	}

	lastSeg := fileSegs[len(fileSegs)-1]
	ext := filepath.Ext(lastSeg)
	lastSegNoExt := strings.TrimSuffix(lastSeg, ext)
	fileSegs[len(fileSegs)-1] = lastSegNoExt

	if len(fileSegs) > 1 {
		parentDir := strings.Join(fileSegs[:len(fileSegs)-1], "/")
		if strings.Contains(strings.Join(segments, "/"), parentDir) {
			return true
		}
	} else {
		if len(segments) > 0 && segments[len(segments)-1] == lastSegNoExt {
			return true
		}
	}
	return false
}

func (idx *Indexer) findFunctionAtLine(filePath string, line int64) string {
	idx.FunctionsMu.RLock()
	fns := idx.FunctionsByFile[filePath]
	idx.FunctionsMu.RUnlock()
	for _, fn := range fns {
		if fn.StartLine <= line && line <= fn.EndLine {
			return fn.ID
		}
	}
	return ""
}

func calculateFileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func detectLanguage(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".java":
		return "java"
	default:
		return ""
	}
}

func findNamePositionInFile(content string, startLine int64, name string) (int64, int64) {
	lines := strings.Split(content, "\n")
	if startLine <= 0 || startLine > int64(len(lines)) {
		return 0, 0
	}
	lineIdx := startLine - 1
	lineContent := lines[lineIdx]
	charIdx := strings.Index(lineContent, name)
	if charIdx < 0 {
		charIdx = 0
	}
	return lineIdx, int64(charIdx)
}
