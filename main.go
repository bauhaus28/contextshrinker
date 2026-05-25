package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"contextshrinker/internal/db"
	"contextshrinker/internal/ignore"
	"contextshrinker/internal/lsp"
	mcp_server "contextshrinker/internal/mcp"
	"contextshrinker/internal/parser"
	"contextshrinker/internal/watcher"
)

var (
	workspaceFlag = flag.String("workspace", ".", "Path to the codebase workspace root")
	dbPathFlag    = flag.String("db", "", "Path to store Kuzu database (defaults to .kuzu in workspace root)")
	visualizeFlag = flag.Bool("visualize", false, "Generate contextshrinker_graph.html visualization and exit immediately")
	searchFlag    = flag.String("search", "", "Search the codebase for definitions matching query and exit immediately")
	structureFlag = flag.String("structure", "", "Get the structure of a file and exit immediately")
	chainFlag     = flag.String("call-chain", "", "Trace upstream callers of a target function name and exit immediately")
	depthFlag     = flag.Int("depth", 3, "Maximum traversal depth for call-chain (1-5)")
	reindexFlag   = flag.Bool("reindex", false, "Force a full workspace ingestion before running query in CLI mode")
)

type Indexer struct {
	workspaceRoot string
	database      *db.Database
	ignoreList    *ignore.IgnoreList
	lspManager    *lsp.LSPManager
	// functionsByFile caches parsed functions indexed by file path for O(1) lookup.
	functionsByFile map[string][]db.FunctionEntity
	functionsMu     sync.RWMutex
}

func main() {
	flag.Parse()

	log.SetOutput(os.Stderr) // Logs must write to stderr to not interfere with stdio JSON-RPC
	log.Println("Starting contextshrinker MCP server...")
	log.Printf("CWD: %s", func() string { dir, _ := os.Getwd(); return dir }())

	isCLIMode := *visualizeFlag || *searchFlag != "" || *structureFlag != "" || *chainFlag != ""

	if isCLIMode {
		// CLI mode: generate visualization and exit immediately
		workspaceRoot, err := filepath.Abs(*workspaceFlag)
		if err != nil {
			log.Fatalf("Invalid workspace path: %v", err)
		}

		// Safety check: Refuse to run or initialize in home directory or root directory
		homeDir, err := os.UserHomeDir()
		if err == nil {
			if workspaceRoot == homeDir || workspaceRoot == "/" {
				log.Fatalf("Refusing to run/initialize contextshrinker in home directory or root directory: %s", workspaceRoot)
			}
		}

		schwobDir := filepath.Join(workspaceRoot, ".contextshrinker")
		if err := os.MkdirAll(schwobDir, 0755); err != nil {
			log.Fatalf("Failed to create .contextshrinker configuration directory: %v", err)
		}

		dbPath := *dbPathFlag
		if dbPath == "" {
			legacyDbPath := filepath.Join(workspaceRoot, ".kuzu")
			if info, err := os.Stat(legacyDbPath); err == nil && info.IsDir() {
				dbPath = legacyDbPath
			} else {
				dbPath = filepath.Join(schwobDir, "db")
			}
		}

		ignoreList, err := ignore.NewIgnoreList(workspaceRoot)
		if err != nil {
			log.Fatalf("Failed to initialize ignore rules: %v", err)
		}

		database, err := db.NewDatabase(dbPath)
		if err != nil {
			log.Fatalf("Failed to initialize database: %v", err)
		}
		defer database.Close()

		var shouldIngest bool
		if *reindexFlag {
			shouldIngest = true
		} else {
			empty, err := database.IsEmpty()
			if err != nil {
				log.Printf("Failed to check if database is empty: %v", err)
				shouldIngest = true
			} else {
				shouldIngest = empty
			}
		}

		if shouldIngest {
			log.Println("Database is empty or reindex was requested. Running workspace ingestion...")
			lspManager := lsp.NewLSPManager(workspaceRoot)
			defer lspManager.Close()

			indexer := &Indexer{
				workspaceRoot:   workspaceRoot,
				database:        database,
				ignoreList:      ignoreList,
				lspManager:      lspManager,
				functionsByFile: make(map[string][]db.FunctionEntity),
			}

			if err := indexer.IngestWorkspace(); err != nil {
				log.Printf("Workspace ingestion encountered errors: %v", err)
			}
			log.Println("Workspace ingestion completed.")
		}

		if *visualizeFlag {
			nodes, edges, err := database.ExportGraph()
			if err != nil {
				log.Fatalf("Failed to export graph data: %v", err)
			}
			path, err := mcp_server.GenerateGraphHTML(workspaceRoot, nodes, edges)
			if err != nil {
				log.Fatalf("Failed to generate HTML visualization: %v", err)
			}
			fmt.Fprintf(os.Stdout, "SUCCESS: Visualization generated at %s\n", path)
			return
		}

		if *searchFlag != "" {
			results, err := database.SearchCodebase(*searchFlag)
			if err != nil {
				log.Fatalf("Search failed: %v", err)
			}
			data, err := json.MarshalIndent(results, "", "  ")
			if err != nil {
				log.Fatalf("Failed to marshal search results: %v", err)
			}
			fmt.Println(string(data))
			return
		}

		if *structureFlag != "" {
			filePath := *structureFlag
			if !filepath.IsAbs(filePath) {
				filePath = filepath.Join(workspaceRoot, filePath)
			}
			filePath, err = filepath.Abs(filePath)
			if err != nil {
				log.Fatalf("Invalid file path: %v", err)
			}
			items, err := database.GetFileStructure(filePath)
			if err != nil {
				log.Fatalf("Failed to retrieve file structure: %v", err)
			}
			data, err := json.MarshalIndent(items, "", "  ")
			if err != nil {
				log.Fatalf("Failed to marshal file structure: %v", err)
			}
			fmt.Println(string(data))
			return
		}

		if *chainFlag != "" {
			links, err := database.GetCallChain(*chainFlag, *depthFlag)
			if err != nil {
				log.Fatalf("Failed to trace call chain: %v", err)
			}
			data, err := json.MarshalIndent(links, "", "  ")
			if err != nil {
				log.Fatalf("Failed to marshal call chain: %v", err)
			}
			fmt.Println(string(data))
			return
		}
	}

	// MCP Mode: initialize variables for lazy loading
	var (
		database         *db.Database
		lspManager       *lsp.LSPManager
		watcherObj       *watcher.Watcher
		initMu           sync.Mutex
		mcpServerWrapper *mcp_server.Server
	)

	// Register defer functions to close resources when main exits
	defer func() {
		initMu.Lock()
		defer initMu.Unlock()
		if watcherObj != nil {
			watcherObj.Close()
		}
		if lspManager != nil {
			lspManager.Close()
		}
		if database != nil {
			database.Close()
		}
	}()

	lazyInit := func(_ context.Context, session *mcp.ServerSession) (*db.Database, error) {
		initMu.Lock()
		defer initMu.Unlock()

		if database != nil {
			return database, nil
		}

		// Retrieve workspace root from client session roots using a timeout to prevent deadlocks
		listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
		rootsRes, err := session.ListRoots(listCtx, nil)
		listCancel()
		if err != nil {
			log.Printf("Failed to list client roots (using timeout context): %v", err)
		}

		workspaceRoot := ""
		if rootsRes != nil && len(rootsRes.Roots) > 0 {
			rootURI := rootsRes.Roots[0].URI
			path, err := lsp.URIToPath(rootURI)
			if err != nil {
				return nil, fmt.Errorf("failed to parse root URI %s: %w", rootURI, err)
			}
			workspaceRoot = path
		} else {
			// Fallback to command-line flag or current directory
			workspaceRoot, err = filepath.Abs(*workspaceFlag)
			if err != nil {
				return nil, fmt.Errorf("invalid default workspace path: %v", err)
			}
		}

		// Safety check: Refuse to run or initialize in home directory or root directory
		homeDir, err := os.UserHomeDir()
		if err == nil {
			if workspaceRoot == homeDir || workspaceRoot == "/" {
				return nil, fmt.Errorf("refusing to run/initialize contextshrinker in home directory or root directory: %s", workspaceRoot)
			}
		}

		schwobDir := filepath.Join(workspaceRoot, ".contextshrinker")
		if info, err := os.Stat(schwobDir); os.IsNotExist(err) {
			log.Printf("Initializing contextshrinker workspace at %s...", workspaceRoot)
			if err := os.MkdirAll(schwobDir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create .contextshrinker configuration directory: %w", err)
			}
		} else if err == nil && !info.IsDir() {
			return nil, fmt.Errorf("path .contextshrinker exists but is not a directory")
		}

		// Set up log file duplication to .contextshrinker/server.log so the user can see logs
		logFilePath := filepath.Join(schwobDir, "server.log")
		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, logFile))
			log.Printf("--- Server session started: CWD=%s Workspace=%s ---", func() string { dir, _ := os.Getwd(); return dir }(), workspaceRoot)
		} else {
			log.Printf("Failed to open server log file %s: %v", logFilePath, err)
		}

		dbPath := *dbPathFlag
		if dbPath == "" {
			legacyDbPath := filepath.Join(workspaceRoot, ".kuzu")
			if info, err := os.Stat(legacyDbPath); err == nil && info.IsDir() {
				dbPath = legacyDbPath
			} else {
				dbPath = filepath.Join(schwobDir, "db")
			}
		}

		// Initialize IgnoreList
		ignoreList, err := ignore.NewIgnoreList(workspaceRoot)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize ignore rules: %w", err)
		}

		// Initialize Database
		database, err = db.NewDatabase(dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize database: %w", err)
		}

		// Unblock pending tool calls immediately since database is ready to be queried
		mcpServerWrapper.SetDatabase(database)

		// Initialize LSP Manager
		lspManager = lsp.NewLSPManager(workspaceRoot)

		indexer := &Indexer{
			workspaceRoot:   workspaceRoot,
			database:        database,
			ignoreList:      ignoreList,
			lspManager:      lspManager,
			functionsByFile: make(map[string][]db.FunctionEntity),
		}

		// Initial Ingestion (Pass 1 & Pass 2)
		log.Println("Performing initial workspace ingestion...")
		if err := indexer.IngestWorkspace(); err != nil {
			log.Printf("Workspace ingestion encountered errors: %v", err)
		}
		log.Println("Workspace ingestion completed.")

		// Initialize Watcher
		watcherObj, err = watcher.NewWatcher(workspaceRoot, ignoreList, func(path string) {
			log.Printf("Delta update triggered for: %s", path)
			indexer.HandleFileUpdate(path)
		})
		if err == nil {
			if err := watcherObj.Start(); err != nil {
				log.Printf("Failed to start file watcher: %v", err)
			} else {
				log.Println("File watcher running.")
			}
		} else {
			log.Printf("Failed to create file watcher: %v", err)
		}

		return database, nil
	}


	mcpServerWrapper = mcp_server.NewServer(func(ctx context.Context, req *mcp.InitializedRequest) {
		log.Println("MCP session initialized, starting background workspace indexing...")
		go func() {
			dbConn, err := lazyInit(ctx, req.Session)
			if err != nil {
				log.Printf("Background workspace indexing failed: %v", err)
			}
			mcpServerWrapper.SetDatabase(dbConn)
		}()
	})

	mcpServer := mcpServerWrapper.GetMCPServer()

	log.Println("contextshrinker server is listening on stdin/stdout...")
	if err := mcpServer.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("MCP Server execution error: %v", err)
	}
}

// IngestWorkspace walks the workspace and performs Pass 1 and Pass 2 on all source files.
func (idx *Indexer) IngestWorkspace() error {
	var files []string
	err := filepath.WalkDir(idx.workspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(idx.workspaceRoot, path)
		if err != nil {
			return nil
		}

		if rel != "." && idx.ignoreList.ShouldIgnore(rel) {
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
	for _, file := range files {
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
	if err := idx.database.DeleteFileEntities(filePath); err != nil {
		return err
	}

	// Insert File Node
	if err := idx.database.InsertFile(filePath, hash, info.ModTime().Unix()); err != nil {
		return err
	}

	// Run Tree-sitter Parse
	res, err := parser.ParseFile(filePath)
	if err != nil {
		return err
	}

	// Insert Functions
	idx.functionsMu.Lock()
	for _, fn := range res.Functions {
		if err := idx.database.InsertFunction(fn); err != nil {
			log.Printf("Failed to insert function %s: %v", fn.Name, err)
			continue
		}
		idx.functionsByFile[filePath] = append(idx.functionsByFile[filePath], fn)
	}
	idx.functionsMu.Unlock()

	// Insert Classes
	for _, c := range res.Classes {
		if err := idx.database.InsertClass(c); err != nil {
			log.Printf("Failed to insert class %s: %v", c.Name, err)
		}
	}

	// Insert Variables
	for _, v := range res.Variables {
		if err := idx.database.InsertVariable(v); err != nil {
			log.Printf("Failed to insert variable %s: %v", v.Name, err)
		}
	}

	// Create CONTAINS Edges
	for _, rel := range res.Contains {
		switch rel.ParentType {
		case "File":
			_ = idx.database.CreateContainsFileToEntity(rel.ParentID, rel.ChildID, rel.ChildType)
		case "Class":
			_ = idx.database.CreateContainsClassToEntity(rel.ParentID, rel.ChildID, rel.ChildType)
		}
	}

	// Create IMPLEMENTS Edges
	for _, rel := range res.Implements {
		// Resolve the ToClass name to a Class ID in DB if possible.
		// For simplicity, we search idx.classes or just assume inheritance name matches.
		// Let's execute a MATCH in Kuzu database to find classes matching ToClassName.
		// If found, link them.
		toClassID := idx.findClassIDByName(rel.ToClassName)
		if toClassID != "" {
			_ = idx.database.CreateImplements(rel.FromClassID, toClassID)
		}
	}

	// Extract and record IMPORTS
	idx.extractAndRecordImports(filePath)

	return nil
}

func (idx *Indexer) runPass2(filePath string) {
	idx.functionsMu.RLock()
	fileFunctions := idx.functionsByFile[filePath]
	idx.functionsMu.RUnlock()

	if len(fileFunctions) == 0 {
		return
	}

	// Get LSP Client
	lang := detectLanguage(filePath)
	client, err := idx.lspManager.GetClient(lang)
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
				_ = idx.database.CreateCalls(callerFnID, fn.ID)
			}
		}
	}
}

// HandleFileUpdate processes a file modification delta update.
func (idx *Indexer) HandleFileUpdate(filePath string) {
	idx.functionsMu.Lock()
	delete(idx.functionsByFile, filePath)
	idx.functionsMu.Unlock()

	// Run Pass 1
	if err := idx.runPass1(filePath); err != nil {
		log.Printf("Delta Pass 1 failed for %s: %v", filePath, err)
		return
	}

	// Run Pass 2
	idx.runPass2(filePath)
}

func (idx *Indexer) findClassIDByName(name string) string {
	// TODO: implement DB lookup by class name to enable IMPLEMENTS edge creation.
	res, err := idx.database.FindClassIDByName(name)
	if err != nil {
		return ""
	}
	return res
}

// extractAndRecordImports is not yet implemented; IMPORTS edges are not populated.
func (idx *Indexer) extractAndRecordImports(_ string) {}

func (idx *Indexer) findFunctionAtLine(filePath string, line int64) string {
	idx.functionsMu.RLock()
	fns := idx.functionsByFile[filePath]
	idx.functionsMu.RUnlock()
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
