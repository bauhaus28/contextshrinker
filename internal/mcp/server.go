package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bauhaus28/contextshrinker/internal/db"
	"github.com/bauhaus28/contextshrinker/internal/report"
)

type Server struct {
	mcpServer     *mcp.Server
	database      *db.Database
	workspaceRoot string
	initDone      chan struct{}
	initOnce      sync.Once
}

type GetArchitectureReportArgs struct{}

type SearchArgs struct {
	Query string `json:"query" jsonschema:"The substring to match against names and docstrings of codebase entities."`
}

type CallChainArgs struct {
	TargetFunction string `json:"target_function" jsonschema:"The name of the target function to trace callers for."`
	Depth          int    `json:"depth,omitempty" jsonschema:"Maximum traversal depth for callers (1-5, defaults to 3)."`
}

type FileStructureArgs struct {
	FilePath string `json:"file_path" jsonschema:"Absolute path of the file to inspect."`
}

// NewServer initializes the MCP server and registers the tools.
func NewServer(initializedHandler func(context.Context, *mcp.InitializedRequest)) *Server {
	s := &Server{
		mcpServer: mcp.NewServer(&mcp.Implementation{
			Name:    "contextshrinker",
			Version: "0.1.1",
		}, &mcp.ServerOptions{
			InitializedHandler: initializedHandler,
		}),
		initDone: make(chan struct{}),
	}

	s.registerTools()

	return s
}

// GetMCPServer returns the underlying MCP server.
func (s *Server) GetMCPServer() *mcp.Server {
	return s.mcpServer
}

// SetDatabase sets the initialized database connection and unblocks pending tool calls.
func (s *Server) SetDatabase(database *db.Database, workspaceRoot string) {
	s.initOnce.Do(func() {
		s.database = database
		s.workspaceRoot = workspaceRoot
		close(s.initDone)
	})
}

func (s *Server) ensureInitialized(ctx context.Context, session *mcp.ServerSession) error {
	select {
	case <-s.initDone:
		if s.database == nil {
			return fmt.Errorf("database initialization failed")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) registerTools() {
	// 1. search_codebase
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "search_codebase",
		Description: "Search the codebase for function, class, and variable definitions matching a query string in their name or docstring.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, any, error) {
		if err := s.ensureInitialized(ctx, req.Session); err != nil {
			return nil, nil, fmt.Errorf("initialization failed: %w", err)
		}

		results, err := s.database.SearchCodebase(args.Query)
		if err != nil {
			return nil, nil, fmt.Errorf("search failed: %w", err)
		}

		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return nil, nil, err
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: string(data)},
			},
		}, nil, nil
	})

	// 2. get_call_chain
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_call_chain",
		Description: "Trace upstream callers of a target function name up to a specified depth.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args CallChainArgs) (*mcp.CallToolResult, any, error) {
		if err := s.ensureInitialized(ctx, req.Session); err != nil {
			return nil, nil, fmt.Errorf("initialization failed: %w", err)
		}

		depth := args.Depth
		if depth <= 0 {
			depth = 3
		}

		links, err := s.database.GetCallChain(args.TargetFunction, depth)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to trace call chain: %w", err)
		}

		data, err := json.MarshalIndent(links, "", "  ")
		if err != nil {
			return nil, nil, err
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: string(data)},
			},
		}, nil, nil
	})

	// 3. get_file_structure
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_file_structure",
		Description: "Get the structure of a file (classes, structures, methods, fields, functions) without reading its full content.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FileStructureArgs) (*mcp.CallToolResult, any, error) {
		if err := s.ensureInitialized(ctx, req.Session); err != nil {
			return nil, nil, fmt.Errorf("initialization failed: %w", err)
		}

		items, err := s.database.GetFileStructure(args.FilePath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to retrieve file structure: %w", err)
		}

		data, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return nil, nil, err
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: string(data)},
			},
		}, nil, nil
	})

	// 4. visualize_codebase
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "visualize_codebase",
		Description: "Generate an interactive, HTML-based visualization of the codebase graph and save it in the workspace root.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args VisualizeArgs) (*mcp.CallToolResult, any, error) {
		if err := s.ensureInitialized(ctx, req.Session); err != nil {
			return nil, nil, fmt.Errorf("initialization failed: %w", err)
		}

		nodes, edges, err := s.database.ExportGraph()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to export graph data: %w", err)
		}

		wRoot := args.WorkspaceRoot
		if wRoot == "" {
			wRoot = s.workspaceRoot
		}

		path, err := GenerateGraphHTML(wRoot, nodes, edges)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to generate HTML file: %w", err)
		}

		resMap := map[string]string{
			"status":    "success",
			"message":   "Codebase graph successfully visualized.",
			"file_path": path,
		}
		data, err := json.MarshalIndent(resMap, "", "  ")
		if err != nil {
			return nil, nil, err
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: string(data)},
			},
		}, nil, nil
	})

	// 5. get_architecture_report
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_architecture_report",
		Description: "Retrieve the codebase architectural health report containing metrics on God Objects, coupling hotspots, cycles, and dead unexported functions.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args GetArchitectureReportArgs) (*mcp.CallToolResult, any, error) {
		if err := s.ensureInitialized(ctx, req.Session); err != nil {
			return nil, nil, fmt.Errorf("initialization failed: %w", err)
		}

		audit, err := report.RunAudit(s.database, s.workspaceRoot)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to execute audit: %w", err)
		}

		markdown := audit.RenderMarkdown(s.workspaceRoot)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: markdown},
			},
		}, nil, nil
	})
}

type VisualizeArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Optional absolute path of the workspace root to save the visualization HTML file to (defaults to indexed workspace root)."`
}
