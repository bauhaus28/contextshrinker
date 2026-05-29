package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bauhaus28/contextshrinker/internal/db"
	"github.com/bauhaus28/contextshrinker/internal/ignore"
	"github.com/bauhaus28/contextshrinker/internal/parser"
)

type DiffNode struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "File", "Function", "Class", "Variable"
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
}

type DiffEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Type  string `json:"type"` // "CALLS", "IMPORTS", "CONTAINS", "IMPLEMENTS"
}

type GraphDiff struct {
	AddedNodes    []DiffNode `json:"added_nodes"`
	DeletedNodes  []DiffNode `json:"deleted_nodes"`
	ModifiedNodes []DiffNode `json:"modified_nodes"`
	AddedEdges    []DiffEdge `json:"added_edges"`
	DeletedEdges  []DiffEdge `json:"deleted_edges"`
}

type targetNode struct {
	StableID string
	Type     string // "File", "Function", "Class", "Variable"
	Name     string
	FilePath string
	ASTHash  string
	Hash     string // Only for File
}

// DiffGraph compares the database state to a historical git commit or branch.
func DiffGraph(ctx context.Context, workspaceRoot string, database *db.Database, ignoreList *ignore.IgnoreList, ref string) (*GraphDiff, error) {
	// 1. Get current workspace data from database
	dbData, err := database.GetDiffData()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve database diff data: %w", err)
	}

	// Maps to lookup current nodes by ID
	currentFiles := make(map[string]db.FileEntity)
	for _, f := range dbData.Files {
		currentFiles[f.Path] = f
	}

	currentFuncs := make(map[string]db.FunctionEntity)
	for _, fn := range dbData.Functions {
		currentFuncs[fn.ID] = fn
	}

	currentClasses := make(map[string]db.ClassEntity)
	for _, c := range dbData.Classes {
		currentClasses[c.ID] = c
	}

	currentVars := make(map[string]db.VariableEntity)
	for _, v := range dbData.Variables {
		currentVars[v.ID] = v
	}

	// Helper function to map database ID/path to a stable ID
	getStableID := func(id, typeStr string) string {
		relPath := ""
		switch typeStr {
		case "File":
			rel, err := filepath.Rel(workspaceRoot, id)
			if err == nil {
				return filepath.ToSlash(rel)
			}
			return filepath.ToSlash(id)
		case "Class":
			if c, ok := currentClasses[id]; ok {
				rel, err := filepath.Rel(workspaceRoot, c.FilePath)
				if err == nil {
					relPath = filepath.ToSlash(rel)
				} else {
					relPath = filepath.ToSlash(c.FilePath)
				}
				return relPath + "::" + c.Name
			}
		case "Function":
			if fn, ok := currentFuncs[id]; ok {
				rel, err := filepath.Rel(workspaceRoot, fn.FilePath)
				if err == nil {
					relPath = filepath.ToSlash(rel)
				} else {
					relPath = filepath.ToSlash(fn.FilePath)
				}
				className := dbData.ContainsRel[id]
				if className != "" {
					return relPath + "::" + className + "::" + fn.Name
				}
				return relPath + "::" + fn.Name
			}
		case "Variable":
			if v, ok := currentVars[id]; ok {
				rel, err := filepath.Rel(workspaceRoot, v.FilePath)
				if err == nil {
					relPath = filepath.ToSlash(rel)
				} else {
					relPath = filepath.ToSlash(v.FilePath)
				}
				className := dbData.ContainsRel[id]
				if className != "" {
					return relPath + "::" + className + "::" + v.Name
				}
				return relPath + "::" + v.Name
			}
		}
		return id
	}

	// Build map of current stable ID -> targetNode representation
	currentStableNodes := make(map[string]targetNode)
	for _, f := range dbData.Files {
		stable := getStableID(f.Path, "File")
		currentStableNodes[stable] = targetNode{
			StableID: stable,
			Type:     "File",
			Name:     filepath.Base(f.Path),
			FilePath: f.Path,
			Hash:     f.Hash,
		}
	}

	for _, fn := range dbData.Functions {
		stable := getStableID(fn.ID, "Function")
		currentStableNodes[stable] = targetNode{
			StableID: stable,
			Type:     "Function",
			Name:     fn.Name,
			FilePath: fn.FilePath,
			ASTHash:  fn.ASTHash,
		}
	}

	for _, c := range dbData.Classes {
		stable := getStableID(c.ID, "Class")
		currentStableNodes[stable] = targetNode{
			StableID: stable,
			Type:     "Class",
			Name:     c.Name,
			FilePath: c.FilePath,
			ASTHash:  c.ASTHash,
		}
	}

	for _, v := range dbData.Variables {
		stable := getStableID(v.ID, "Variable")
		currentStableNodes[stable] = targetNode{
			StableID: stable,
			Type:     "Variable",
			Name:     v.Name,
			FilePath: v.FilePath,
		}
	}

	// Map to track current edges as stable pairs
	currentStableEdges := make(map[string]DiffEdge)
	for _, edge := range dbData.Edges {
		var fromType, toType string

		// Determine From node type
		if _, ok := currentFiles[edge.From]; ok {
			fromType = "File"
		} else if _, ok := currentClasses[edge.From]; ok {
			fromType = "Class"
		} else if _, ok := currentFuncs[edge.From]; ok {
			fromType = "Function"
		} else if _, ok := currentVars[edge.From]; ok {
			fromType = "Variable"
		}

		// Determine To node type
		if _, ok := currentFiles[edge.To]; ok {
			toType = "File"
		} else if _, ok := currentClasses[edge.To]; ok {
			toType = "Class"
		} else if _, ok := currentFuncs[edge.To]; ok {
			toType = "Function"
		} else if _, ok := currentVars[edge.To]; ok {
			toType = "Variable"
		}

		fromStable := getStableID(edge.From, fromType)
		toStable := getStableID(edge.To, toType)
		key := fromStable + "->" + toStable + ":" + edge.Label
		currentStableEdges[key] = DiffEdge{
			From: fromStable,
			To:   toStable,
			Type: edge.Label,
		}
	}

	// 2. Fetch and parse target Git reference
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-r", "--name-only", ref)
	cmd.Dir = workspaceRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to list files for Git ref %q: %v (stderr: %s)", ref, err, stderr.String())
	}

	rawFiles := strings.Split(stdout.String(), "\n")
	var targetSourceFiles []string
	for _, f := range rawFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if ignoreList.ShouldIgnore(f) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f))
		if ext == ".go" || ext == ".py" || ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx" || ext == ".java" {
			targetSourceFiles = append(targetSourceFiles, f)
		}
	}

	// Target structures
	targetStableNodes := make(map[string]targetNode)
	targetStableEdges := make(map[string]DiffEdge)

	// We need absolute paths to match database style File Paths
	for _, relPath := range targetSourceFiles {
		absPath := filepath.Join(workspaceRoot, relPath)
		cmdShow := exec.CommandContext(ctx, "git", "show", ref+":"+relPath)
		cmdShow.Dir = workspaceRoot
		var showOut, showErr bytes.Buffer
		cmdShow.Stdout = &showOut
		cmdShow.Stderr = &showErr
		if err := cmdShow.Run(); err != nil {
			// Skip files that failed to show
			continue
		}

		content := showOut.Bytes()
		h := sha256.New()
		h.Write(content)
		fileHash := hex.EncodeToString(h.Sum(nil))

		// Parse the target file content
		parsed, err := parser.ParseContent(content, filepath.Ext(relPath), absPath)
		if err != nil {
			continue
		}

		// Map class IDs to Class Names in target
		classIDToName := make(map[string]string)
		for _, c := range parsed.Classes {
			classIDToName[c.ID] = c.Name
		}

		childToClassName := make(map[string]string)
		for _, rel := range parsed.Contains {
			if rel.ParentType == "Class" {
				if name, ok := classIDToName[rel.ParentID]; ok {
					childToClassName[rel.ChildID] = name
				}
			}
		}

		// Helper to get target stable ID
		getTargetStableID := func(id, typeStr, name string) string {
			cleanRel := filepath.ToSlash(relPath)
			switch typeStr {
			case "File":
				return cleanRel
			case "Class":
				return cleanRel + "::" + name
			case "Function", "Variable":
				className := childToClassName[id]
				if className != "" {
					return cleanRel + "::" + className + "::" + name
				}
				return cleanRel + "::" + name
			}
			return id
		}

		// Add File
		fileStable := filepath.ToSlash(relPath)
		targetStableNodes[fileStable] = targetNode{
			StableID: fileStable,
			Type:     "File",
			Name:     filepath.Base(relPath),
			FilePath: absPath,
			Hash:     fileHash,
		}

		// Add Functions
		for _, fn := range parsed.Functions {
			stable := getTargetStableID(fn.ID, "Function", fn.Name)
			targetStableNodes[stable] = targetNode{
				StableID: stable,
				Type:     "Function",
				Name:     fn.Name,
				FilePath: fn.FilePath,
				ASTHash:  fn.ASTHash,
			}
		}

		// Add Classes
		for _, c := range parsed.Classes {
			stable := getTargetStableID(c.ID, "Class", c.Name)
			targetStableNodes[stable] = targetNode{
				StableID: stable,
				Type:     "Class",
				Name:     c.Name,
				FilePath: c.FilePath,
				ASTHash:  c.ASTHash,
			}
		}

		// Add Variables
		for _, v := range parsed.Variables {
			stable := getTargetStableID(v.ID, "Variable", v.Name)
			targetStableNodes[stable] = targetNode{
				StableID: stable,
				Type:     "Variable",
				Name:     v.Name,
				FilePath: v.FilePath,
			}
		}

		// Map to translate parsed ID to stable ID (for edge translation)
		translateID := func(id, typeStr, name string) string {
			if typeStr == "File" {
				return filepath.ToSlash(relPath)
			}
			return getTargetStableID(id, typeStr, name)
		}

		// Build target maps to look up parsed entities by ID
		parsedFuncs := make(map[string]db.FunctionEntity)
		for _, fn := range parsed.Functions {
			parsedFuncs[fn.ID] = fn
		}
		parsedClasses := make(map[string]db.ClassEntity)
		for _, c := range parsed.Classes {
			parsedClasses[c.ID] = c
		}
		parsedVars := make(map[string]db.VariableEntity)
		for _, v := range parsed.Variables {
			parsedVars[v.ID] = v
		}

		// Parse CONTAINS edges
		for _, rel := range parsed.Contains {
			var fromStable, toStable string
			if rel.ParentType == "File" {
				fromStable = filepath.ToSlash(relPath)
			} else if rel.ParentType == "Class" {
				if c, ok := parsedClasses[rel.ParentID]; ok {
					fromStable = translateID(rel.ParentID, "Class", c.Name)
				}
			}

			if rel.ChildType == "Class" {
				if c, ok := parsedClasses[rel.ChildID]; ok {
					toStable = translateID(rel.ChildID, "Class", c.Name)
				}
			} else if rel.ChildType == "Function" {
				if fn, ok := parsedFuncs[rel.ChildID]; ok {
					toStable = translateID(rel.ChildID, "Function", fn.Name)
				}
			} else if rel.ChildType == "Variable" {
				if v, ok := parsedVars[rel.ChildID]; ok {
					toStable = translateID(rel.ChildID, "Variable", v.Name)
				}
			}

			if fromStable != "" && toStable != "" {
				key := fromStable + "->" + toStable + ":CONTAINS"
				targetStableEdges[key] = DiffEdge{From: fromStable, To: toStable, Type: "CONTAINS"}
			}
		}

		// Parse IMPORTS edges
		for _, imp := range parsed.Imports {
			targets := resolveImportTargetTarget(workspaceRoot, targetSourceFiles, imp.SourcePath, imp.Path)
			for _, target := range targets {
				if target != imp.SourcePath {
					fromStable := filepath.ToSlash(relPath)
					relTarget, err := filepath.Rel(workspaceRoot, target)
					if err == nil {
						toStable := filepath.ToSlash(relTarget)
						key := fromStable + "->" + toStable + ":IMPORTS"
						targetStableEdges[key] = DiffEdge{From: fromStable, To: toStable, Type: "IMPORTS"}
					}
				}
			}
		}

		// Parse IMPLEMENTS edges
		for _, rel := range parsed.Implements {
			if fromClass, ok := parsedClasses[rel.FromClassID]; ok {
				fromStable := translateID(rel.FromClassID, "Class", fromClass.Name)
				// Find target class ID by name
				var toStable string
				for _, tc := range parsed.Classes {
					if tc.Name == rel.ToClassName {
						toStable = translateID(tc.ID, "Class", tc.Name)
						break
					}
				}
				if toStable != "" {
					key := fromStable + "->" + toStable + ":IMPLEMENTS"
					targetStableEdges[key] = DiffEdge{From: fromStable, To: toStable, Type: "IMPLEMENTS"}
				}
			}
		}
	}

	// 3. Compute Differences
	diff := &GraphDiff{}

	// Added / Modified Nodes
	for stable, curr := range currentStableNodes {
		tgt, exists := targetStableNodes[stable]
		if !exists {
			diff.AddedNodes = append(diff.AddedNodes, DiffNode{
				ID:       curr.StableID,
				Type:     curr.Type,
				Name:     curr.Name,
				FilePath: curr.FilePath,
			})
		} else {
			modified := false
			if curr.Type == "File" {
				if curr.Hash != tgt.Hash {
					modified = true
				}
			} else if curr.Type == "Function" || curr.Type == "Class" {
				if curr.ASTHash != tgt.ASTHash {
					modified = true
				}
			}
			if modified {
				diff.ModifiedNodes = append(diff.ModifiedNodes, DiffNode{
					ID:       curr.StableID,
					Type:     curr.Type,
					Name:     curr.Name,
					FilePath: curr.FilePath,
				})
			}
		}
	}

	// Deleted Nodes
	for stable, tgt := range targetStableNodes {
		if _, exists := currentStableNodes[stable]; !exists {
			diff.DeletedNodes = append(diff.DeletedNodes, DiffNode{
				ID:       tgt.StableID,
				Type:     tgt.Type,
				Name:     tgt.Name,
				FilePath: tgt.FilePath,
			})
		}
	}

	// Added Edges
	for key, currEdge := range currentStableEdges {
		if _, exists := targetStableEdges[key]; !exists {
			// Don't flag CALLS edges as added if they are just LSP relations
			// since target Git reference doesn't have CALLS edges from LSP.
			if currEdge.Type != "CALLS" {
				diff.AddedEdges = append(diff.AddedEdges, currEdge)
			}
		}
	}

	// Deleted Edges
	for key, tgtEdge := range targetStableEdges {
		if _, exists := currentStableEdges[key]; !exists {
			if tgtEdge.Type != "CALLS" {
				diff.DeletedEdges = append(diff.DeletedEdges, tgtEdge)
			}
		}
	}

	return diff, nil
}

// resolveImportTargetTarget resolves target imports similarly to resolveImportTarget in indexer.go
func resolveImportTargetTarget(workspaceRoot string, targetFiles []string, sourceFile, importPath string) []string {
	importPath = filepath.ToSlash(importPath)
	var targets []string

	// 1. Relative imports
	if strings.HasPrefix(importPath, ".") {
		absDir := filepath.Join(filepath.Dir(sourceFile), importPath)
		extensions := []string{".go", ".py", ".js", ".jsx", ".ts", ".tsx", ".java"}
		for _, ext := range extensions {
			testPath := absDir + ext
			// Check if this path matches any target file
			for _, tf := range targetFiles {
				if filepath.Join(workspaceRoot, tf) == testPath {
					targets = append(targets, testPath)
					return targets
				}
			}
		}
		// If directory check
		// (For simplicity, we check if any target file's absolute path starts with absDir)
		for _, tf := range targetFiles {
			absTf := filepath.Join(workspaceRoot, tf)
			if strings.HasPrefix(absTf, absDir) {
				targets = append(targets, absTf)
			}
		}
		if len(targets) > 0 {
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

	for _, tf := range targetFiles {
		if matchImportToWorkspaceFile(tf, segments) {
			targets = append(targets, filepath.Join(workspaceRoot, tf))
		}
	}

	return targets
}
