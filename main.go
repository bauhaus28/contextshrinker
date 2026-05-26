package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bauhaus28/contextshrinker/internal/db"
	"github.com/bauhaus28/contextshrinker/internal/ignore"
	"github.com/bauhaus28/contextshrinker/internal/indexer"
	"github.com/bauhaus28/contextshrinker/internal/lsp"
	mcp_server "github.com/bauhaus28/contextshrinker/internal/mcp"
	"github.com/bauhaus28/contextshrinker/internal/watcher"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runMCPServer() {
	var (
		database         *db.Database
		lspManager       *lsp.LSPManager
		watcherObj       *watcher.Watcher
		initMu           sync.Mutex
		mcpServerWrapper *mcp_server.Server
		workspaceRoot    string
	)

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

		workspaceRoot = ""
		if rootsRes != nil && len(rootsRes.Roots) > 0 {
			rootURI := rootsRes.Roots[0].URI
			path, err := lsp.URIToPath(rootURI)
			if err != nil {
				return nil, fmt.Errorf("failed to parse root URI %s: %w", rootURI, err)
			}
			workspaceRoot = path
		} else {
			// Fallback to command-line flag or current directory
			var err error
			workspaceRoot, err = filepath.Abs(workspacePath)
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

		resolvedDbPath := dbPath
		if resolvedDbPath == "" {
			legacyDbPath := filepath.Join(workspaceRoot, ".kuzu")
			if info, err := os.Stat(legacyDbPath); err == nil && info.IsDir() {
				resolvedDbPath = legacyDbPath
			} else {
				resolvedDbPath = filepath.Join(schwobDir, "db")
			}
		}

		// Initialize IgnoreList
		ignoreList, err := ignore.NewIgnoreList(workspaceRoot)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize ignore rules: %w", err)
		}

		// Initialize Database
		database, err = db.NewDatabase(resolvedDbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize database: %w", err)
		}

		// Unblock pending tool calls immediately since database is ready to be queried
		mcpServerWrapper.SetDatabase(database, workspaceRoot)

		// Initialize LSP Manager
		lspManager = lsp.NewLSPManager(workspaceRoot)

		idx := indexer.NewIndexer(workspaceRoot, database, ignoreList, lspManager)

		// Initial Ingestion (Pass 1 & Pass 2)
		log.Println("Performing initial workspace ingestion...")
		if err := idx.IngestWorkspace(); err != nil {
			log.Printf("Workspace ingestion encountered errors: %v", err)
		}
		log.Println("Workspace ingestion completed.")

		// Initialize Watcher
		watcherObj, err = watcher.NewWatcher(workspaceRoot, ignoreList, func(path string) {
			log.Printf("Delta update triggered for: %s", path)
			idx.HandleFileUpdate(path)
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
			mcpServerWrapper.SetDatabase(dbConn, workspaceRoot)
		}()
	})

	mcpServer := mcpServerWrapper.GetMCPServer()

	log.Println("contextshrinker server is listening on stdin/stdout...")
	if err := mcpServer.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("MCP Server execution error: %v", err)
	}
}
