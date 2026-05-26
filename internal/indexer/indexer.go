package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"contextshrinker/internal/db"
	"contextshrinker/internal/ignore"
	"contextshrinker/internal/lsp"
	"contextshrinker/internal/parser"
)

type Indexer struct {
	WorkspaceRoot   string
	Database        *db.Database
	IgnoreList      *ignore.IgnoreList
	LspManager      *lsp.LSPManager
	FunctionsByFile map[string][]db.FunctionEntity
	FunctionsMu     sync.RWMutex
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

	// Pass 1: Tree-sitter scan
	log.Printf("Pass 1: Syntax parsing %d files...", len(files))
	for i, file := range files {
		if (i+1)%500 == 0 || i == 0 || i == len(files)-1 {
			log.Printf("Pass 1: [%d/%d] Ingesting %s...", i+1, len(files), filepath.Base(file))
		}
		if err := idx.runPass1(file); err != nil {
			log.Printf("Pass 1 failed for %s: %v", file, err)
		}
	}

	// Pass 2: LSP semantic indexing
	log.Printf("Pass 2: Building relational call graph...")
	for _, file := range files {
		idx.runPass2(file)
	}

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

	// Delete old references for the file first (idempotent delta)
	if err := idx.Database.DeleteFileEntities(filePath); err != nil {
		return err
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
	idx.extractAndRecordImports(filePath)

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
		log.Printf("Skipping Pass 2 for %s: LSP not available (%v)", filePath, err)
		return
	}

	// Read file content
	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	contentStr := string(contentBytes)

	// Open file in LSP
	_ = client.DidOpen(filePath, contentStr)

	// Query references for each function
	for _, fn := range fileFunctions {
		line, char := findNamePositionInFile(contentStr, fn.StartLine, fn.Name)
		locs, err := client.References(context.Background(), filePath, line, char)
		if err != nil {
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
				_ = idx.Database.CreateCalls(callerFnID, fn.ID)
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

// extractAndRecordImports is not yet implemented; IMPORTS edges are not populated.
func (idx *Indexer) extractAndRecordImports(_ string) {}

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
