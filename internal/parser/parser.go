package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"contextshrinker/internal/db"
)

type ParsedResult struct {
	FilePath   string
	Functions  []db.FunctionEntity
	Classes    []db.ClassEntity
	Variables  []db.VariableEntity
	Contains   []ContainsRel
	Implements []ImplementsRel
}

type ContainsRel struct {
	ParentID   string // Can be File path or Class ID
	ChildID    string
	ParentType string // "File" or "Class"
	ChildType  string // "Function", "Class", "Variable"
}

type ImplementsRel struct {
	FromClassID string
	ToClassName string // Name of interface/class it extends/implements
}

// ParseFile parses a source file using Tree-sitter and returns extracted entities.
func ParseFile(filePath string) (*ParsedResult, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	var lang *sitter.Language

	switch ext {
	case ".go":
		lang = golang.GetLanguage()
	case ".py":
		lang = python.GetLanguage()
	case ".js", ".jsx", ".mjs", ".cjs":
		lang = javascript.GetLanguage()
	case ".ts", ".tsx":
		lang = typescript.GetLanguage()
	case ".java":
		lang = java.GetLanguage()
	default:
		return nil, fmt.Errorf("unsupported file extension: %s", ext)
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse content: %w", err)
	}

	res := &ParsedResult{
		FilePath: filePath,
	}

	walkAST(tree.RootNode(), content, res, "")
	return res, nil
}

func walkAST(node *sitter.Node, source []byte, res *ParsedResult, currentClassID string) {
	if node == nil {
		return
	}

	nodeType := node.Type()
	startPoint := node.StartPoint()
	endPoint := node.EndPoint()

	switch nodeType {
	case "function_declaration", "method_declaration", "function_definition", "method_definition":
		// Extract function/method
		name := extractChildName(node, source)
		if name != "" {
			id := fmt.Sprintf("%s:%d:%s", res.FilePath, startPoint.Row+1, name)
			doc := getPrecedingComment(node, source)

			isExported := true
			if res.FilePath != "" && strings.HasSuffix(res.FilePath, ".go") {
				// Go export rules
				r := []rune(name)
				if len(r) > 0 {
					isExported = unicode.IsUpper(r[0])
				}
			}

			fn := db.FunctionEntity{
				ID:         id,
				Name:       name,
				FilePath:   res.FilePath,
				StartLine:  int64(startPoint.Row + 1),
				EndLine:    int64(endPoint.Row + 1),
				Docstring:  doc,
				IsExported: isExported,
			}
			res.Functions = append(res.Functions, fn)

			// Record CONTAINS relationship
			if currentClassID != "" {
				res.Contains = append(res.Contains, ContainsRel{
					ParentID:   currentClassID,
					ChildID:    id,
					ParentType: "Class",
					ChildType:  "Function",
				})
			} else {
				res.Contains = append(res.Contains, ContainsRel{
					ParentID:   res.FilePath,
					ChildID:    id,
					ParentType: "File",
					ChildType:  "Function",
				})
			}
		}

	case "class_declaration", "class_definition", "interface_declaration", "struct_spec", "type_spec":
		// Extract class/struct/interface
		name := extractChildName(node, source)
		// For Go type_spec, if we have type struct/interface
		if nodeType == "type_spec" {
			structOrInterface := false
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child.Type() == "struct_type" || child.Type() == "interface_type" {
					structOrInterface = true
					break
				}
			}
			if !structOrInterface {
				break
			}
		}

		if name != "" {
			id := fmt.Sprintf("%s:%d:%s", res.FilePath, startPoint.Row+1, name)
			doc := getPrecedingComment(node, source)

			category := "class"
			if strings.Contains(nodeType, "interface") {
				category = "interface"
			} else if nodeType == "type_spec" {
				category = "struct"
			}

			c := db.ClassEntity{
				ID:           id,
				Name:         name,
				FilePath:     res.FilePath,
				Docstring:    doc,
				TypeCategory: category,
			}
			res.Classes = append(res.Classes, c)

			// File contains class
			res.Contains = append(res.Contains, ContainsRel{
				ParentID:   res.FilePath,
				ChildID:    id,
				ParentType: "File",
				ChildType:  "Class",
			})

			// Check heritage/inheritance in Java, Kotlin, TS
			extractInheritance(node, source, id, res)

			// Recurse into class body to parse methods/fields, update currentClassID
			for i := 0; i < int(node.ChildCount()); i++ {
				walkAST(node.Child(i), source, res, id)
			}
			return
		}

	case "variable_declarator", "var_spec", "const_spec", "field_declaration":
		// Extract variables
		name := extractChildName(node, source)
		if name != "" {
			id := fmt.Sprintf("%s:%d:%s", res.FilePath, startPoint.Row+1, name)
			typeHint := ""
			typeNode := node.ChildByFieldName("type")
			if typeNode != nil {
				typeHint = string(source[typeNode.StartByte():typeNode.EndByte()])
			}

			v := db.VariableEntity{
				ID:       id,
				Name:     name,
				FilePath: res.FilePath,
				TypeHint: typeHint,
			}
			res.Variables = append(res.Variables, v)

			if currentClassID != "" {
				res.Contains = append(res.Contains, ContainsRel{
					ParentID:   currentClassID,
					ChildID:    id,
					ParentType: "Class",
					ChildType:  "Variable",
				})
			} else {
				res.Contains = append(res.Contains, ContainsRel{
					ParentID:   res.FilePath,
					ChildID:    id,
					ParentType: "File",
					ChildType:  "Variable",
				})
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		walkAST(node.Child(i), source, res, currentClassID)
	}
}

func extractChildName(node *sitter.Node, source []byte) string {
	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		return strings.TrimSpace(string(source[nameNode.StartByte():nameNode.EndByte()]))
	}

	// Fallback to searching for identifier or declarator in child nodes
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		t := child.Type()
		if t == "identifier" || t == "field_identifier" || t == "type_identifier" {
			return strings.TrimSpace(string(source[child.StartByte():child.EndByte()]))
		}
		if t == "variable_declarator" || t == "function_declarator" {
			return extractChildName(child, source)
		}
	}

	return ""
}

func getPrecedingComment(node *sitter.Node, source []byte) string {
	var comments []string
	curr := node.PrevSibling()
	for curr != nil {
		t := curr.Type()
		if t == "comment" || t == "line_comment" || t == "block_comment" {
			text := strings.TrimSpace(string(source[curr.StartByte():curr.EndByte()]))
			text = strings.TrimPrefix(text, "//")
			text = strings.TrimPrefix(text, "/*")
			text = strings.TrimSuffix(text, "*/")
			text = strings.TrimSpace(text)
			comments = append([]string{text}, comments...)
			curr = curr.PrevSibling()
		} else if curr.StartByte() == curr.EndByte() { // empty/whitespace node
			curr = curr.PrevSibling()
		} else {
			break
		}
	}
	return strings.Join(comments, "\n")
}

func extractInheritance(node *sitter.Node, source []byte, classID string, res *ParsedResult) {
	nodeType := node.Type()

	// In TypeScript / JavaScript
	if nodeType == "class_declaration" || nodeType == "interface_declaration" {
		// look for heritage_clause
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "heritage_clause" {
				heritageText := string(source[child.StartByte():child.EndByte()])
				// E.g., "extends BaseClass implements Interface1"
				words := strings.Fields(heritageText)
				for j, w := range words {
					if (w == "extends" || w == "implements") && j+1 < len(words) {
						res.Implements = append(res.Implements, ImplementsRel{
							FromClassID: classID,
							ToClassName: strings.TrimSuffix(words[j+1], ","),
						})
					}
				}
			}
		}
	}

	// In Java / Kotlin
	if nodeType == "class_declaration" {
		// look for extends/implements
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			ct := child.Type()
			if ct == "superclass" || ct == "interfaces" || ct == "type_list" {
				// Get names from the type list or superclass
				text := string(source[child.StartByte():child.EndByte()])
				text = strings.ReplaceAll(text, "extends", "")
				text = strings.ReplaceAll(text, "implements", "")
				parts := strings.Split(text, ",")
				for _, part := range parts {
					name := strings.TrimSpace(part)
					if name != "" {
						res.Implements = append(res.Implements, ImplementsRel{
							FromClassID: classID,
							ToClassName: name,
						})
					}
				}
			}
		}
	}
}
