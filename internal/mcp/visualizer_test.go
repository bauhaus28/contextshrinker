package mcp

import (
	"os"
	"strings"
	"testing"

	"github.com/bauhaus28/contextshrinker/internal/db"
)

func TestGenerateGraphHTML(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "contextshrinker-visualizer-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	nodes := []db.VisNode{
		{ID: "node1", Label: "main.go", Group: "File", Title: "main.go file"},
		{ID: "node2", Label: "main", Group: "Function", Title: "main function"},
	}
	edges := []db.VisEdge{
		{From: "node1", To: "node2", Label: "CONTAINS"},
	}

	path, err := GenerateGraphHTML(tmpDir, nodes, edges)
	if err != nil {
		t.Fatalf("failed to generate HTML: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("output file does not exist: %v", err)
	}

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	content := string(contentBytes)

	if !strings.Contains(content, "vis.Network") {
		t.Error("expected output to contain 'vis.Network'")
	}
	if !strings.Contains(content, `"node1"`) || !strings.Contains(content, `"node2"`) {
		t.Error("expected output to embed node IDs")
	}
	if !strings.Contains(content, `"CONTAINS"`) {
		t.Error("expected output to embed edge label")
	}
}
