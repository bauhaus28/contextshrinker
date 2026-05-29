package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/bauhaus28/contextshrinker/internal/db"
	"github.com/bauhaus28/contextshrinker/internal/ignore"
	"github.com/bauhaus28/contextshrinker/internal/indexer"
	"github.com/bauhaus28/contextshrinker/internal/lsp"
	"github.com/bauhaus28/contextshrinker/internal/dashboard"
	mcp_server "github.com/bauhaus28/contextshrinker/internal/mcp"
	"github.com/bauhaus28/contextshrinker/internal/report"
)

var (
	workspacePath string
	dbPath        string
	reindexForce  bool
	formatType    string
	depthLimit    int
	dashboardPort int
)

var rootCmd = &cobra.Command{
	Use:   "contextshrinker",
	Short: "ContextShrinker MCP Server and architectural assessment tool",
	Long:  `ContextShrinker is a headless MCP server and CLI utility that parses your codebase into an embedded Kùzu graph database to analyze architecture and reduce LLM context token bloat.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Default fallback action when no subcommands are matched: start MCP server
		runMCPServer()
	},
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Model Context Protocol (MCP) server daemon",
	Run: func(cmd *cobra.Command, args []string) {
		runMCPServer()
	},
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a contextshrinker project and generate .csignore",
	Run: func(cmd *cobra.Command, args []string) {
		runInit()
	},
}

var analyzeCmd = &cobra.Command{
	Use:   "analyze",
	Short: "Analyze the codebase for architectural design quality and output a report",
	Run: func(cmd *cobra.Command, args []string) {
		runAnalyze()
	},
}

var promptCmd = &cobra.Command{
	Use:   "prompt",
	Short: "General prompt templates command",
}

var architectCmd = &cobra.Command{
	Use:   "architect",
	Short: "Print the Principal Systems Architect system prompt",
	Run: func(cmd *cobra.Command, args []string) {
		printArchitectPrompt()
	},
}

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search the codebase for definitions matching query",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runSearch(args[0])
	},
}

var structureCmd = &cobra.Command{
	Use:   "structure [file_path]",
	Short: "Get the syntax structure of a file",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runStructure(args[0])
	},
}

var callChainCmd = &cobra.Command{
	Use:   "call-chain [function_name]",
	Short: "Trace upstream callers of a target function name",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runCallChain(args[0])
	},
}

var visualizeCmd = &cobra.Command{
	Use:   "visualize",
	Short: "Generate an interactive HTML codebase visualization",
	Run: func(cmd *cobra.Command, args []string) {
		runVisualize()
	},
}

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Start the interactive architectural audit and explanation dashboard",
	Run: func(cmd *cobra.Command, args []string) {
		runDashboard()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&workspacePath, "workspace", ".", "Path to the codebase workspace root")
	rootCmd.PersistentFlags().StringVar(&dbPath, "db", "", "Path to store Kuzu database (defaults to .contextshrinker/db)")
	rootCmd.PersistentFlags().BoolVar(&reindexForce, "reindex", false, "Force a full workspace ingestion before running query")

	analyzeCmd.Flags().StringVar(&formatType, "format", "markdown", "Output format (markdown)")
	callChainCmd.Flags().IntVar(&depthLimit, "depth", 3, "Maximum traversal depth for callers (1-5)")
	dashboardCmd.Flags().IntVar(&dashboardPort, "port", 8080, "Port to run the dashboard server on")

	promptCmd.AddCommand(architectCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(analyzeCmd)
	rootCmd.AddCommand(promptCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(structureCmd)
	rootCmd.AddCommand(callChainCmd)
	rootCmd.AddCommand(visualizeCmd)
	rootCmd.AddCommand(dashboardCmd)
}


func getDBAndIngestIfNeeded(workspaceRoot string) (*db.Database, error) {
	schwobDir := filepath.Join(workspaceRoot, ".contextshrinker")
	if err := os.MkdirAll(schwobDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create .contextshrinker configuration directory: %w", err)
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

	ignoreList, err := ignore.NewIgnoreList(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize ignore rules: %w", err)
	}

	if reindexForce {
		log.Printf("Reindex requested. Clearing existing database at %s...", resolvedDbPath)
		_ = os.RemoveAll(resolvedDbPath)
	}

	database, err := db.NewDatabase(resolvedDbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	var shouldIngest bool
	if reindexForce {
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
		if database.IsReadOnly() {
			database.Close()
			return nil, fmt.Errorf("database is locked by another process (read-only mode) and cannot be reindexed")
		}
		log.Println("Database is empty or reindex was requested. Running workspace ingestion...")
		lspManager := lsp.NewLSPManager(workspaceRoot)
		defer lspManager.Close()

		idx := indexer.NewIndexer(workspaceRoot, database, ignoreList, lspManager)

		if err := idx.IngestWorkspace(); err != nil {
			log.Printf("Workspace ingestion encountered errors: %v", err)
		}
		log.Println("Workspace ingestion completed.")
	}

	return database, nil
}

func runAnalyze() {
	workspaceRoot, err := filepath.Abs(workspacePath)
	if err != nil {
		log.Fatalf("Invalid workspace path: %v", err)
	}

	// Safety check
	homeDir, err := os.UserHomeDir()
	if err == nil {
		if workspaceRoot == homeDir || workspaceRoot == "/" {
			log.Fatalf("Refusing to run contextshrinker in home directory or root directory: %s", workspaceRoot)
		}
	}

	database, err := getDBAndIngestIfNeeded(workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	log.Println("Analyzing codebase architecture...")

	audit, err := report.RunAudit(database, workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to execute audit: %v", err)
	}

	markdownReport := audit.RenderMarkdown(workspaceRoot)

	reportPath := filepath.Join(workspaceRoot, "contextshrinker-report.md")
	err = os.WriteFile(reportPath, []byte(markdownReport), 0644)
	if err != nil {
		log.Fatalf("Failed to write report file: %v", err)
	}

	fmt.Printf("SUCCESS: Architectural health report written to %s\n", reportPath)
}

func runInit() {
	workspaceRoot, err := filepath.Abs(workspacePath)
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

	csignorePath := filepath.Join(workspaceRoot, ".csignore")
	if _, err := os.Stat(csignorePath); os.IsNotExist(err) {
		defaultIgnoreContent := `# Custom ignore patterns for contextshrinker
# Each line is a pattern matched recursively.
# Standard defaults (node_modules, vendor, testdata, *.test, etc.) are ignored automatically.

.vitepress
`
		if err := os.WriteFile(csignorePath, []byte(defaultIgnoreContent), 0644); err != nil {
			log.Fatalf("Failed to create .csignore file at %s: %v", csignorePath, err)
		}
		fmt.Printf("SUCCESS: Initialized contextshrinker workspace at %s\nCreated %s\n", workspaceRoot, csignorePath)
	} else {
		fmt.Printf("Workspace already initialized. %s already exists.\n", csignorePath)
	}
}


func printArchitectPrompt() {
	prompt := `# Role
You are a Principal Systems Architect specializing in Domain-Driven Design (DDD) and codebase modularity. Your goal is to analyze the provided structural graph of our application and propose high-level architectural improvements.

# Context
You have access to the codebase graph, which explicitly maps 'CALLS', 'IMPORTS', and 'CONTAINS' relationships. Do not guess the architecture; rely strictly on the edges and nodes provided.

# Objective 1: Assess Current State
1. Identify structural bottlenecks (e.g., God Objects, tightly coupled utility files).
2. Identify circular dependencies and explain the structural risk they pose.
3. Evaluate cohesion: Are files in the same directory calling each other (high cohesion), or are they constantly calling across directory boundaries (high coupling)?

# Objective 2: Propose Meaningful Splits (Project/Service Extraction)
Do not just suggest renaming variables or moving functions. Identify distinct "Bounded Contexts" hidden within the monolith. Look for Graph Clusters: groups of classes and functions that have dense 'CALLS' edges between each other, but very few 'CALLS' edges to the rest of the application.

If you find a cluster:
1. Define the logical domain of this cluster (e.g., "Authentication Service", "Data Ingestion Pipeline").
2. Propose a structural split: Explain exactly which files/folders should be extracted into a separate standalone project or Go module.
3. Define the API Boundary: If this cluster is extracted, what are the exact 2 or 3 functions that will need to become public API endpoints or interfaces to communicate with the rest of the system?

# Output
Provide a structured Markdown response prioritizing the highest-impact architectural changes. Be ruthless in identifying poor separation of concerns.
`
	fmt.Print(prompt)
}

func runSearch(query string) {
	workspaceRoot, err := filepath.Abs(workspacePath)
	if err != nil {
		log.Fatalf("Invalid workspace path: %v", err)
	}

	database, err := getDBAndIngestIfNeeded(workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	results, err := database.SearchCodebase(query)
	if err != nil {
		log.Fatalf("Search failed: %v", err)
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal search results: %v", err)
	}
	fmt.Println(string(data))
}

func runStructure(filePath string) {
	workspaceRoot, err := filepath.Abs(workspacePath)
	if err != nil {
		log.Fatalf("Invalid workspace path: %v", err)
	}

	database, err := getDBAndIngestIfNeeded(workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	absPath := filePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(workspaceRoot, absPath)
	}
	absPath, err = filepath.Abs(absPath)
	if err != nil {
		log.Fatalf("Invalid file path: %v", err)
	}

	items, err := database.GetFileStructure(absPath)
	if err != nil {
		log.Fatalf("Failed to retrieve file structure: %v", err)
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal file structure: %v", err)
	}
	fmt.Println(string(data))
}

func runCallChain(funcName string) {
	workspaceRoot, err := filepath.Abs(workspacePath)
	if err != nil {
		log.Fatalf("Invalid workspace path: %v", err)
	}

	database, err := getDBAndIngestIfNeeded(workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	links, err := database.GetCallChain(funcName, depthLimit)
	if err != nil {
		log.Fatalf("Failed to trace call chain: %v", err)
	}
	data, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal call chain: %v", err)
	}
	fmt.Println(string(data))
}

func runVisualize() {
	workspaceRoot, err := filepath.Abs(workspacePath)
	if err != nil {
		log.Fatalf("Invalid workspace path: %v", err)
	}

	database, err := getDBAndIngestIfNeeded(workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	nodes, edges, err := database.ExportGraph()
	if err != nil {
		log.Fatalf("Failed to export graph data: %v", err)
	}
	path, err := mcp_server.GenerateGraphHTML(workspaceRoot, nodes, edges)
	if err != nil {
		log.Fatalf("Failed to generate HTML visualization: %v", err)
	}
	fmt.Fprintf(os.Stdout, "SUCCESS: Visualization generated at %s\n", path)
}

func runDashboard() {
	workspaceRoot, err := filepath.Abs(workspacePath)
	if err != nil {
		log.Fatalf("Invalid workspace path: %v", err)
	}

	database, err := getDBAndIngestIfNeeded(workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	ignoreList, err := ignore.NewIgnoreList(workspaceRoot)
	if err != nil {
		log.Fatalf("Failed to initialize ignore rules: %v", err)
	}

	err = dashboard.StartServer(workspaceRoot, database, ignoreList, dashboardPort)
	if err != nil {
		log.Fatalf("Dashboard server failed: %v", err)
	}
}

