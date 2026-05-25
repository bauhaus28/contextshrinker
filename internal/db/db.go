package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kuzu "github.com/kuzudb/go-kuzu"
)

// Database wraps the Kùzu database instance and its connection.
type Database struct {
	db   *kuzu.Database
	conn *kuzu.Connection
}

// Entity types for mapping in Go code
type FunctionEntity struct {
	ID         string
	Name       string
	FilePath   string
	StartLine  int64
	EndLine    int64
	Docstring  string
	IsExported bool
}

type ClassEntity struct {
	ID           string
	Name         string
	FilePath     string
	Docstring    string
	TypeCategory string // e.g. "class", "struct", "interface"
}

type VariableEntity struct {
	ID       string
	Name     string
	FilePath string
	TypeHint string
}

type SearchResult struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	FilePath  string `json:"file_path"`
	StartLine int64  `json:"start_line,omitempty"`
}

type CallChainLink struct {
	CallerName string `json:"caller_name"`
	CallerFile string `json:"caller_file"`
	CalleeName string `json:"callee_name"`
	CalleeFile string `json:"callee_file"`
}

type FileStructureItem struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	ClassName string `json:"class_name,omitempty"`
	StartLine int64  `json:"start_line,omitempty"`
	EndLine   int64  `json:"end_line,omitempty"`
}

// NewDatabase opens the Kùzu database at the specified path and runs DDL schema initialization.
func NewDatabase(dbPath string) (*Database, error) {
	// Create parent directory if not exist (Kuzu requires leaf directory not to pre-exist)
	parent := filepath.Dir(dbPath)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database parent directory: %w", err)
	}

	config := kuzu.DefaultSystemConfig()
	// Disable compression if necessary or stick to defaults
	db, err := kuzu.OpenDatabase(dbPath, config)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	conn, err := kuzu.OpenConnection(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to open connection: %w", err)
	}

	d := &Database{db: db, conn: conn}
	if err := d.initSchema(); err != nil {
		conn.Close()
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return d, nil
}

// Close closes the connection and database.
func (d *Database) Close() {
	if d.conn != nil {
		d.conn.Close()
	}
	if d.db != nil {
		d.db.Close()
	}
}

// IsEmpty returns true if there are no File nodes in the database.
func (d *Database) IsEmpty() (bool, error) {
	stmt, err := d.conn.Prepare(`MATCH (f:File) RETURN count(f) AS cnt`)
	if err != nil {
		return true, err
	}
	defer stmt.Close()
	res, err := d.conn.Execute(stmt, nil)
	if err != nil {
		return true, err
	}
	defer res.Close()
	if res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			return true, err
		}
		defer tuple.Close()
		m, err := tuple.GetAsMap()
		if err != nil {
			return true, err
		}
		return mapInt64(m, "cnt") == 0, nil
	}
	return true, nil
}

// runDDL ignores "already exists" errors.
func (d *Database) runDDL(query string) error {
	res, err := d.conn.Query(query)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "Duplicate") {
			return nil
		}
		return err
	}
	res.Close()
	return nil
}

func (d *Database) initSchema() error {
	// 1. Create Node Tables
	nodesDDL := []string{
		`CREATE NODE TABLE File (path STRING, hash STRING, last_modified INT64, PRIMARY KEY (path))`,
		`CREATE NODE TABLE Function (id STRING, name STRING, file_path STRING, start_line INT64, end_line INT64, docstring STRING, is_exported BOOLEAN, PRIMARY KEY (id))`,
		`CREATE NODE TABLE Class (id STRING, name STRING, file_path STRING, docstring STRING, type_category STRING, PRIMARY KEY (id))`,
		`CREATE NODE TABLE Variable (id STRING, name STRING, file_path STRING, type_hint STRING, PRIMARY KEY (id))`,
	}

	for _, ddl := range nodesDDL {
		if err := d.runDDL(ddl); err != nil {
			return fmt.Errorf("failed creating node table: %w", err)
		}
	}

	// 2. Create Relationship (Rel) Tables
	relsDDL := []string{
		`CREATE REL TABLE CALLS (FROM Function TO Function, MANY_MANY)`,
		`CREATE REL TABLE IMPORTS (FROM File TO File, FROM File TO Class, FROM File TO Function, FROM File TO Variable, MANY_MANY)`,
		`CREATE REL TABLE CONTAINS (FROM File TO Function, FROM File TO Class, FROM File TO Variable, FROM Class TO Function, FROM Class TO Variable, MANY_MANY)`,
		`CREATE REL TABLE IMPLEMENTS (FROM Class TO Class, MANY_MANY)`,
	}

	for _, ddl := range relsDDL {
		if err := d.runDDL(ddl); err != nil {
			return fmt.Errorf("failed creating relationship table: %w", err)
		}
	}

	return nil
}

// DeleteFileEntities deletes all nodes and relationships associated with a file path (delta update).
func (d *Database) DeleteFileEntities(filePath string) error {
	queries := []string{
		`MATCH (fn:Function) WHERE fn.file_path = $path DETACH DELETE fn`,
		`MATCH (c:Class) WHERE c.file_path = $path DETACH DELETE c`,
		`MATCH (v:Variable) WHERE v.file_path = $path DETACH DELETE v`,
		`MATCH (f:File {path: $path}) DETACH DELETE f`,
	}

	for _, query := range queries {
		stmt, err := d.conn.Prepare(query)
		if err != nil {
			return err
		}
		res, err := d.conn.Execute(stmt, map[string]any{"path": filePath})
		stmt.Close()
		if err != nil {
			return err
		}
		res.Close()
	}
	return nil
}

// InsertFile inserts a file node.
func (d *Database) InsertFile(path, hash string, lastModified int64) error {
	stmt, err := d.conn.Prepare(`CREATE (f:File {path: $path, hash: $hash, last_modified: $last_modified})`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"path":          path,
		"hash":          hash,
		"last_modified": lastModified,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// InsertFunction inserts a function node.
func (d *Database) InsertFunction(fn FunctionEntity) error {
	stmt, err := d.conn.Prepare(`CREATE (fn:Function {id: $id, name: $name, file_path: $file_path, start_line: $start_line, end_line: $end_line, docstring: $docstring, is_exported: $is_exported})`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"id":          fn.ID,
		"name":        fn.Name,
		"file_path":   fn.FilePath,
		"start_line":  fn.StartLine,
		"end_line":    fn.EndLine,
		"docstring":   fn.Docstring,
		"is_exported": fn.IsExported,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// InsertClass inserts a class node.
func (d *Database) InsertClass(c ClassEntity) error {
	stmt, err := d.conn.Prepare(`CREATE (c:Class {id: $id, name: $name, file_path: $file_path, docstring: $docstring, type_category: $type_category})`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"id":            c.ID,
		"name":          c.Name,
		"file_path":     c.FilePath,
		"docstring":     c.Docstring,
		"type_category": c.TypeCategory,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// InsertVariable inserts a variable node.
func (d *Database) InsertVariable(v VariableEntity) error {
	stmt, err := d.conn.Prepare(`CREATE (v:Variable {id: $id, name: $name, file_path: $file_path, type_hint: $type_hint})`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"id":        v.ID,
		"name":      v.Name,
		"file_path": v.FilePath,
		"type_hint": v.TypeHint,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

var allowedEntityTypes = map[string]bool{"Function": true, "Class": true, "Variable": true}

func validateEntityType(entityType string) error {
	if !allowedEntityTypes[entityType] {
		return fmt.Errorf("invalid entity type: %q", entityType)
	}
	return nil
}

// CreateContainsFileToEntity creates a CONTAINS edge from File to Function/Class/Variable.
func (d *Database) CreateContainsFileToEntity(filePath, entityID, entityType string) error {
	if err := validateEntityType(entityType); err != nil {
		return err
	}
	query := fmt.Sprintf(`MATCH (f:File {path: $file_path}), (e:%s {id: $entity_id}) CREATE (f)-[:CONTAINS]->(e)`, entityType)
	stmt, err := d.conn.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"file_path": filePath,
		"entity_id": entityID,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// CreateContainsClassToEntity creates a CONTAINS edge from Class to Function/Variable.
func (d *Database) CreateContainsClassToEntity(classID, entityID, entityType string) error {
	if err := validateEntityType(entityType); err != nil {
		return err
	}
	query := fmt.Sprintf(`MATCH (c:Class {id: $class_id}), (e:%s {id: $entity_id}) CREATE (c)-[:CONTAINS]->(e)`, entityType)
	stmt, err := d.conn.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"class_id":  classID,
		"entity_id": entityID,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// CreateCalls creates a CALLS edge.
func (d *Database) CreateCalls(callerID, calleeID string) error {
	stmt, err := d.conn.Prepare(`MATCH (caller:Function {id: $caller_id}), (callee:Function {id: $callee_id}) CREATE (caller)-[:CALLS]->(callee)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"caller_id": callerID,
		"callee_id": calleeID,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// CreateImportsFileToFile creates an IMPORTS edge between files.
func (d *Database) CreateImportsFileToFile(fromPath, toPath string) error {
	stmt, err := d.conn.Prepare(`MATCH (f1:File {path: $from_path}), (f2:File {path: $to_path}) CREATE (f1)-[:IMPORTS]->(f2)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"from_path": fromPath,
		"to_path":   toPath,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// CreateImportsFileToEntity creates an IMPORTS edge from File to Class/Function/Variable.
func (d *Database) CreateImportsFileToEntity(fromPath, entityID, entityType string) error {
	if err := validateEntityType(entityType); err != nil {
		return err
	}
	query := fmt.Sprintf(`MATCH (f:File {path: $from_path}), (e:%s {id: $entity_id}) CREATE (f)-[:IMPORTS]->(e)`, entityType)
	stmt, err := d.conn.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"from_path": fromPath,
		"entity_id": entityID,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// FindClassIDByName returns the ID of the first Class with the given name, or "" if not found.
func (d *Database) FindClassIDByName(name string) (string, error) {
	stmt, err := d.conn.Prepare(`MATCH (c:Class {name: $name}) RETURN c.id LIMIT 1`)
	if err != nil {
		return "", err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{"name": name})
	if err != nil {
		return "", err
	}
	defer res.Close()

	if res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			return "", err
		}
		defer tuple.Close()
		m, err := tuple.GetAsMap()
		if err != nil {
			return "", err
		}
		return mapStr(m, "c.id"), nil
	}
	return "", nil
}

// CreateImplements creates an IMPLEMENTS edge between Classes/Interfaces.
func (d *Database) CreateImplements(fromID, toID string) error {
	stmt, err := d.conn.Prepare(`MATCH (c1:Class {id: $from_id}), (c2:Class {id: $to_id}) CREATE (c1)-[:IMPLEMENTS]->(c2)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{
		"from_id": fromID,
		"to_id":   toID,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

func mapStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func mapInt64(m map[string]any, key string) int64 {
	v, _ := m[key].(int64)
	return v
}

// SearchCodebase executes search over functions, classes, and variables.
func (d *Database) SearchCodebase(query string) ([]SearchResult, error) {
	var results []SearchResult
	var firstErr error

	// Function Search
	if stmt, err := d.conn.Prepare(`MATCH (fn:Function) WHERE fn.name CONTAINS $query OR fn.docstring CONTAINS $query RETURN fn.name, fn.file_path, fn.start_line`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"query": query}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						results = append(results, SearchResult{
							Type:      "Function",
							Name:      mapStr(m, "fn.name"),
							FilePath:  mapStr(m, "fn.file_path"),
							StartLine: mapInt64(m, "fn.start_line"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		} else if firstErr == nil {
			firstErr = err
		}
		stmt.Close()
	} else if firstErr == nil {
		firstErr = err
	}

	// Class Search
	if stmt, err := d.conn.Prepare(`MATCH (c:Class) WHERE c.name CONTAINS $query OR c.docstring CONTAINS $query RETURN c.name, c.file_path`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"query": query}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						results = append(results, SearchResult{
							Type:     "Class",
							Name:     mapStr(m, "c.name"),
							FilePath: mapStr(m, "c.file_path"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		} else if firstErr == nil {
			firstErr = err
		}
		stmt.Close()
	} else if firstErr == nil {
		firstErr = err
	}

	// Variable Search
	if stmt, err := d.conn.Prepare(`MATCH (v:Variable) WHERE v.name CONTAINS $query RETURN v.name, v.file_path`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"query": query}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						results = append(results, SearchResult{
							Type:     "Variable",
							Name:     mapStr(m, "v.name"),
							FilePath: mapStr(m, "v.file_path"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		} else if firstErr == nil {
			firstErr = err
		}
		stmt.Close()
	} else if firstErr == nil {
		firstErr = err
	}

	return results, firstErr
}

// GetCallChain traces all upstream callers of the target function up to depth.
func (d *Database) GetCallChain(targetFunction string, depth int) ([]CallChainLink, error) {
	if depth < 1 {
		depth = 1
	} else if depth > 5 {
		depth = 5
	}

	query := fmt.Sprintf(`MATCH (caller:Function)-[:CALLS*1..%d]->(callee:Function) WHERE callee.name = $name RETURN caller.name, caller.file_path, callee.name, callee.file_path`, depth)
	stmt, err := d.conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{"name": targetFunction})
	if err != nil {
		return nil, err
	}
	defer res.Close()

	var links []CallChainLink
	for res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			continue
		}
		m, err := tuple.GetAsMap()
		if err == nil {
			links = append(links, CallChainLink{
				CallerName: mapStr(m, "caller.name"),
				CallerFile: mapStr(m, "caller.file_path"),
				CalleeName: mapStr(m, "callee.name"),
				CalleeFile: mapStr(m, "callee.file_path"),
			})
		}
		tuple.Close()
	}

	return links, nil
}

// GetFileStructure returns the hierarchical contents of a file.
func (d *Database) GetFileStructure(filePath string) ([]FileStructureItem, error) {
	var items []FileStructureItem

	// 1. Top-level Functions
	if stmt, err := d.conn.Prepare(`MATCH (f:File {path: $path})-[:CONTAINS]->(fn:Function) RETURN fn.name, fn.start_line, fn.end_line`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"path": filePath}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						items = append(items, FileStructureItem{
							Type:      "Function",
							Name:      mapStr(m, "fn.name"),
							StartLine: mapInt64(m, "fn.start_line"),
							EndLine:   mapInt64(m, "fn.end_line"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		}
		stmt.Close()
	}

	// 2. Top-level Classes
	if stmt, err := d.conn.Prepare(`MATCH (f:File {path: $path})-[:CONTAINS]->(c:Class) RETURN c.name`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"path": filePath}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						items = append(items, FileStructureItem{
							Type: "Class",
							Name: mapStr(m, "c.name"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		}
		stmt.Close()
	}

	// 3. Top-level Variables
	if stmt, err := d.conn.Prepare(`MATCH (f:File {path: $path})-[:CONTAINS]->(v:Variable) RETURN v.name`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"path": filePath}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						items = append(items, FileStructureItem{
							Type: "Variable",
							Name: mapStr(m, "v.name"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		}
		stmt.Close()
	}

	// 4. Methods within Classes
	if stmt, err := d.conn.Prepare(`MATCH (f:File {path: $path})-[:CONTAINS]->(c:Class)-[:CONTAINS]->(fn:Function) RETURN c.name, fn.name, fn.start_line, fn.end_line`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"path": filePath}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						items = append(items, FileStructureItem{
							Type:      "Method",
							Name:      mapStr(m, "fn.name"),
							ClassName: mapStr(m, "c.name"),
							StartLine: mapInt64(m, "fn.start_line"),
							EndLine:   mapInt64(m, "fn.end_line"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		}
		stmt.Close()
	}

	// 5. Fields within Classes
	if stmt, err := d.conn.Prepare(`MATCH (f:File {path: $path})-[:CONTAINS]->(c:Class)-[:CONTAINS]->(v:Variable) RETURN c.name, v.name`); err == nil {
		if res, err := d.conn.Execute(stmt, map[string]any{"path": filePath}); err == nil {
			for res.HasNext() {
				if tuple, err := res.Next(); err == nil {
					if m, err := tuple.GetAsMap(); err == nil {
						items = append(items, FileStructureItem{
							Type:      "Field",
							Name:      mapStr(m, "v.name"),
							ClassName: mapStr(m, "c.name"),
						})
					}
					tuple.Close()
				}
			}
			res.Close()
		}
		stmt.Close()
	}

	return items, nil
}

type VisNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group"`
	Title string `json:"title,omitempty"`
}

type VisEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
}

// queryRows runs a raw query and calls fn for each row's map, skipping rows with errors.
func (d *Database) queryRows(query string, fn func(map[string]any)) {
	res, err := d.conn.Query(query)
	if err != nil {
		return
	}
	defer res.Close()
	for res.HasNext() {
		t, err := res.Next()
		if err != nil {
			continue
		}
		m, err := t.GetAsMap()
		if err == nil {
			fn(m)
		}
		t.Close()
	}
}

func (d *Database) ExportGraph() ([]VisNode, []VisEdge, error) {
	var nodes []VisNode
	var edges []VisEdge

	d.queryRows(`MATCH (f:File) RETURN f.path`, func(m map[string]any) {
		p := mapStr(m, "f.path")
		nodes = append(nodes, VisNode{ID: p, Label: filepath.Base(p), Group: "File", Title: "File: " + p})
	})

	d.queryRows(`MATCH (fn:Function) RETURN fn.id, fn.name, fn.docstring`, func(m map[string]any) {
		nodes = append(nodes, VisNode{
			ID:    mapStr(m, "fn.id"),
			Label: mapStr(m, "fn.name"),
			Group: "Function",
			Title: "Function: " + mapStr(m, "fn.name") + "\n" + mapStr(m, "fn.docstring"),
		})
	})

	d.queryRows(`MATCH (c:Class) RETURN c.id, c.name, c.docstring`, func(m map[string]any) {
		nodes = append(nodes, VisNode{
			ID:    mapStr(m, "c.id"),
			Label: mapStr(m, "c.name"),
			Group: "Class",
			Title: "Class: " + mapStr(m, "c.name") + "\n" + mapStr(m, "c.docstring"),
		})
	})

	d.queryRows(`MATCH (v:Variable) RETURN v.id, v.name, v.type_hint`, func(m map[string]any) {
		nodes = append(nodes, VisNode{
			ID:    mapStr(m, "v.id"),
			Label: mapStr(m, "v.name"),
			Group: "Variable",
			Title: "Variable: " + mapStr(m, "v.name") + " (" + mapStr(m, "v.type_hint") + ")",
		})
	})

	edgeQueries := []struct {
		query      string
		fromKey    string
		toKey      string
		label      string
	}{
		{`MATCH (a:Function)-[:CALLS]->(b:Function) RETURN a.id, b.id`, "a.id", "b.id", "CALLS"},
		{`MATCH (a:File)-[:CONTAINS]->(b:Function) RETURN a.path, b.id`, "a.path", "b.id", "CONTAINS"},
		{`MATCH (a:File)-[:CONTAINS]->(b:Class) RETURN a.path, b.id`, "a.path", "b.id", "CONTAINS"},
		{`MATCH (a:File)-[:CONTAINS]->(b:Variable) RETURN a.path, b.id`, "a.path", "b.id", "CONTAINS"},
		{`MATCH (a:Class)-[:CONTAINS]->(b:Function) RETURN a.id, b.id`, "a.id", "b.id", "CONTAINS"},
		{`MATCH (a:Class)-[:CONTAINS]->(b:Variable) RETURN a.id, b.id`, "a.id", "b.id", "CONTAINS"},
		{`MATCH (a:File)-[:IMPORTS]->(b:File) RETURN a.path, b.path`, "a.path", "b.path", "IMPORTS"},
		{`MATCH (a:File)-[:IMPORTS]->(b:Class) RETURN a.path, b.id`, "a.path", "b.id", "IMPORTS"},
		{`MATCH (a:File)-[:IMPORTS]->(b:Function) RETURN a.path, b.id`, "a.path", "b.id", "IMPORTS"},
		{`MATCH (a:File)-[:IMPORTS]->(b:Variable) RETURN a.path, b.id`, "a.path", "b.id", "IMPORTS"},
		{`MATCH (a:Class)-[:IMPLEMENTS]->(b:Class) RETURN a.id, b.id`, "a.id", "b.id", "IMPLEMENTS"},
	}
	for _, eq := range edgeQueries {
		eq := eq
		d.queryRows(eq.query, func(m map[string]any) {
			edges = append(edges, VisEdge{
				From:  mapStr(m, eq.fromKey),
				To:    mapStr(m, eq.toKey),
				Label: eq.label,
			})
		})
	}

	return nodes, edges, nil
}
