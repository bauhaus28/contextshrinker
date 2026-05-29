package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	kuzu "github.com/kuzudb/go-kuzu"
)

// Database wraps the Kùzu database instance and its connection.
type Database struct {
	db         *kuzu.Database
	conn       *kuzu.Connection
	isReadOnly bool
	mu         sync.Mutex
}

// IsReadOnly returns true if the database was opened in read-only mode.
func (d *Database) IsReadOnly() bool {
	return d.isReadOnly
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
	ASTHash    string
}

type ClassEntity struct {
	ID           string
	Name         string
	FilePath     string
	Docstring    string
	TypeCategory string // e.g. "class", "struct", "interface"
	ASTHash      string
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
	isReadOnly := false
	db, err := kuzu.OpenDatabase(dbPath, config)
	if err != nil {
		// Fallback to read-only mode if read-write failed (e.g. database is locked by another process)
		config.ReadOnly = true
		var fallbackErr error
		db, fallbackErr = kuzu.OpenDatabase(dbPath, config)
		if fallbackErr != nil {
			return nil, fmt.Errorf("failed to open database: %w (fallback read-only failed: %v)", err, fallbackErr)
		}
		isReadOnly = true
	}

	conn, err := kuzu.OpenConnection(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to open connection: %w", err)
	}

	d := &Database{db: db, conn: conn, isReadOnly: isReadOnly}
	if !isReadOnly {
		if err := d.initSchema(); err != nil {
			conn.Close()
			db.Close()
			return nil, fmt.Errorf("failed to initialize schema: %w", err)
		}
	}

	return d, nil
}

// Close closes the connection and database.
func (d *Database) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		d.conn.Close()
	}
	if d.db != nil {
		d.db.Close()
	}
}

// IsEmpty returns true if there are no File nodes in the database.
func (d *Database) IsEmpty() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
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

// FileExists returns true if a File node with the given path exists in the database.
func (d *Database) FileExists(path string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	stmt, err := d.conn.Prepare(`MATCH (f:File {path: $path}) RETURN count(f) AS cnt`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{"path": path})
	if err != nil {
		return false, err
	}
	defer res.Close()

	if res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			return false, err
		}
		defer tuple.Close()
		m, err := tuple.GetAsMap()
		if err != nil {
			return false, err
		}
		return mapInt64(m, "cnt") > 0, nil
	}
	return false, nil
}

// runDDL ignores "already exists" errors.
func (d *Database) runDDL(query string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
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
		`CREATE NODE TABLE Function (id STRING, name STRING, file_path STRING, start_line INT64, end_line INT64, docstring STRING, is_exported BOOLEAN, ast_hash STRING, PRIMARY KEY (id))`,
		`CREATE NODE TABLE Class (id STRING, name STRING, file_path STRING, docstring STRING, type_category STRING, ast_hash STRING, PRIMARY KEY (id))`,
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

	// 3. Migrate schemas (adds missing columns if database already exists)
	migrations := []string{
		`ALTER TABLE Function ADD ast_hash STRING`,
		`ALTER TABLE Class ADD ast_hash STRING`,
	}
	for _, m := range migrations {
		_ = d.runDDL(m) // ignore errors if column already exists or is duplicate
	}

	return nil
}

// DeleteFileEntities deletes all nodes and relationships associated with a file path (delta update).
func (d *Database) DeleteFileEntities(filePath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
	stmt, err := d.conn.Prepare(`CREATE (fn:Function {id: $id, name: $name, file_path: $file_path, start_line: $start_line, end_line: $end_line, docstring: $docstring, is_exported: $is_exported, ast_hash: $ast_hash})`)
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
		"ast_hash":    fn.ASTHash,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// InsertClass inserts a class node.
func (d *Database) InsertClass(c ClassEntity) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	stmt, err := d.conn.Prepare(`CREATE (c:Class {id: $id, name: $name, file_path: $file_path, docstring: $docstring, type_category: $type_category, ast_hash: $ast_hash})`)
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
		"ast_hash":      c.ASTHash,
	})
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// InsertVariable inserts a variable node.
func (d *Database) InsertVariable(v VariableEntity) error {
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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


// SearchCodebase executes search over functions, classes, and variables.
func (d *Database) SearchCodebase(query string) ([]SearchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
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
		} else {
			firstErr = err
		}
		stmt.Close()
	} else {
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
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

type GodObjectResult struct {
	ClassName          string
	FilePath           string
	OutboundComplexity int64
}

type BlackHoleResult struct {
	FunctionName        string
	FilePath            string
	InboundDependencies int64
}

type CycleResult struct {
	FilePath string
}

type DeadCodeResult struct {
	FunctionName string
	FilePath     string
}

// GetGodObjects returns classes with high outbound complexity (contains many methods/variables).
func (d *Database) GetGodObjects() ([]GodObjectResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var results []GodObjectResult
	stmt, err := d.conn.Prepare(`MATCH (c:Class)-[r:CONTAINS]->() RETURN c.name, c.file_path, count(r) AS OutboundComplexity ORDER BY OutboundComplexity DESC LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	res, err := d.conn.Execute(stmt, nil)
	if err != nil {
		return nil, err
	}
	defer res.Close()

	for res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			continue
		}
		m, err := tuple.GetAsMap()
		if err == nil {
			results = append(results, GodObjectResult{
				ClassName:          mapStr(m, "c.name"),
				FilePath:           mapStr(m, "c.file_path"),
				OutboundComplexity: mapInt64(m, "OutboundComplexity"),
			})
		}
		tuple.Close()
	}
	return results, nil
}

// GetBlackHoles returns functions with high inbound dependencies (heavily called).
func (d *Database) GetBlackHoles() ([]BlackHoleResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var results []BlackHoleResult
	stmt, err := d.conn.Prepare(`MATCH ()-[r:CALLS]->(f:Function) RETURN f.name, f.file_path, count(r) AS InboundDependencies ORDER BY InboundDependencies DESC LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	res, err := d.conn.Execute(stmt, nil)
	if err != nil {
		return nil, err
	}
	defer res.Close()

	for res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			continue
		}
		m, err := tuple.GetAsMap()
		if err == nil {
			results = append(results, BlackHoleResult{
				FunctionName:        mapStr(m, "f.name"),
				FilePath:            mapStr(m, "f.file_path"),
				InboundDependencies: mapInt64(m, "InboundDependencies"),
			})
		}
		tuple.Close()
	}
	return results, nil
}

// GetCycles detects cyclic dependencies (circular imports between files).
func (d *Database) GetCycles() ([]CycleResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var results []CycleResult
	stmt, err := d.conn.Prepare(`MATCH (a:File)-[:IMPORTS*1..5]->(a) RETURN a.path LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	res, err := d.conn.Execute(stmt, nil)
	if err != nil {
		return nil, err
	}
	defer res.Close()

	for res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			continue
		}
		m, err := tuple.GetAsMap()
		if err == nil {
			results = append(results, CycleResult{
				FilePath: mapStr(m, "a.path"),
			})
		}
		tuple.Close()
	}
	return results, nil
}

// GetDeadCode detects functions that are not called and are not exported.
func (d *Database) GetDeadCode() ([]DeadCodeResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var results []DeadCodeResult
	stmt, err := d.conn.Prepare(`MATCH (f:Function) WHERE NOT ()-[:CALLS]->(f) AND f.is_exported = false RETURN f.name, f.file_path`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	res, err := d.conn.Execute(stmt, nil)
	if err != nil {
		return nil, err
	}
	defer res.Close()

	for res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			continue
		}
		m, err := tuple.GetAsMap()
		if err == nil {
			results = append(results, DeadCodeResult{
				FunctionName: mapStr(m, "f.name"),
				FilePath:     mapStr(m, "f.file_path"),
			})
		}
		tuple.Close()
	}
	return results, nil
}

type CloneGroup struct {
	ASTHash   string         `json:"ast_hash"`
	Functions []SearchResult `json:"functions"`
}

type BoundaryRule struct {
	FromPattern string `json:"from_pattern"`
	ToPattern   string `json:"to_pattern"`
}

type BoundaryViolation struct {
	Type         string `json:"type"` // "CALL" or "IMPORT"
	FromPath     string `json:"from_path"`
	FromSymbol   string `json:"from_symbol,omitempty"`
	ToPath       string `json:"to_path"`
	ToSymbol     string `json:"to_symbol,omitempty"`
	RuleViolated string `json:"rule_violated"`
}

// GetClones finds functions that have the exact same structural AST hash.
func (d *Database) GetClones() ([]CloneGroup, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	query := `MATCH (fn:Function)
WHERE fn.ast_hash <> "" AND fn.ast_hash IS NOT NULL
WITH fn.ast_hash AS hash, count(fn) AS cnt
WHERE cnt > 1
MATCH (fn2:Function {ast_hash: hash})
RETURN fn2.name, fn2.file_path, fn2.start_line, hash
ORDER BY hash`

	stmt, err := d.conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, nil)
	if err != nil {
		return nil, err
	}
	defer res.Close()

	groupsMap := make(map[string][]SearchResult)
	for res.HasNext() {
		tuple, err := res.Next()
		if err != nil {
			continue
		}
		m, err := tuple.GetAsMap()
		if err == nil {
			hash := mapStr(m, "hash")
			groupsMap[hash] = append(groupsMap[hash], SearchResult{
				Type:      "Function",
				Name:      mapStr(m, "fn2.name"),
				FilePath:  mapStr(m, "fn2.file_path"),
				StartLine: mapInt64(m, "fn2.start_line"),
			})
		}
		tuple.Close()
	}

	var cloneGroups []CloneGroup
	for hash, functions := range groupsMap {
		cloneGroups = append(cloneGroups, CloneGroup{
			ASTHash:   hash,
			Functions: functions,
		})
	}
	return cloneGroups, nil
}

// GetBoundaryViolations identifies calls and imports that violate layer/module restrictions.
func (d *Database) GetBoundaryViolations(rules []BoundaryRule) ([]BoundaryViolation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var violations []BoundaryViolation

	// 1. Check CALLS edges
	stmtCalls, err := d.conn.Prepare(`MATCH (fn1:Function)-[:CALLS]->(fn2:Function) RETURN fn1.name, fn1.file_path, fn1.start_line, fn2.name, fn2.file_path`)
	if err == nil {
		resCalls, err := d.conn.Execute(stmtCalls, nil)
		if err == nil {
			for resCalls.HasNext() {
				tuple, err := resCalls.Next()
				if err != nil {
					continue
				}
				m, err := tuple.GetAsMap()
				if err == nil {
					f1 := mapStr(m, "fn1.file_path")
					f2 := mapStr(m, "fn2.file_path")
					for _, rule := range rules {
						if strings.Contains(f1, rule.FromPattern) && strings.Contains(f2, rule.ToPattern) {
							violations = append(violations, BoundaryViolation{
								Type:         "CALL",
								FromPath:     f1,
								FromSymbol:   mapStr(m, "fn1.name"),
								ToPath:       f2,
								ToSymbol:     mapStr(m, "fn2.name"),
								RuleViolated: fmt.Sprintf("%s -> %s", rule.FromPattern, rule.ToPattern),
							})
						}
					}
				}
				tuple.Close()
			}
			resCalls.Close()
		}
		stmtCalls.Close()
	}

	// 2. Check IMPORTS edges
	stmtImports, err := d.conn.Prepare(`MATCH (f1:File)-[:IMPORTS]->(f2:File) RETURN f1.path, f2.path`)
	if err == nil {
		resImports, err := d.conn.Execute(stmtImports, nil)
		if err == nil {
			for resImports.HasNext() {
				tuple, err := resImports.Next()
				if err != nil {
					continue
				}
				m, err := tuple.GetAsMap()
				if err == nil {
					f1 := mapStr(m, "f1.path")
					f2 := mapStr(m, "f2.path")
					for _, rule := range rules {
						if strings.Contains(f1, rule.FromPattern) && strings.Contains(f2, rule.ToPattern) {
							violations = append(violations, BoundaryViolation{
								Type:         "IMPORT",
								FromPath:     f1,
								ToPath:       f2,
								RuleViolated: fmt.Sprintf("%s -> %s", rule.FromPattern, rule.ToPattern),
							})
						}
					}
				}
				tuple.Close()
			}
			resImports.Close()
		}
		stmtImports.Close()
	}

	return violations, nil
}

// GetStats returns total function count, total class count, interface count, and concrete class count.
func (d *Database) GetStats() (int64, int64, int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var funcCount, classCount, interfaceCount, concreteCount int64

	stmt1, err := d.conn.Prepare(`MATCH (fn:Function) RETURN count(fn) AS cnt`)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer stmt1.Close()
	res1, err := d.conn.Execute(stmt1, nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer res1.Close()
	if res1.HasNext() {
		t, err := res1.Next()
		if err == nil && t != nil {
			m, err := t.GetAsMap()
			if err == nil {
				funcCount = mapInt64(m, "cnt")
			}
			t.Close()
		}
	}

	stmt2, err := d.conn.Prepare(`MATCH (c:Class) RETURN count(c) AS cnt`)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer stmt2.Close()
	res2, err := d.conn.Execute(stmt2, nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer res2.Close()
	if res2.HasNext() {
		t, err := res2.Next()
		if err == nil && t != nil {
			m, err := t.GetAsMap()
			if err == nil {
				classCount = mapInt64(m, "cnt")
			}
			t.Close()
		}
	}

	stmt3, err := d.conn.Prepare(`MATCH (c:Class) WHERE c.type_category = 'interface' RETURN count(c) AS cnt`)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer stmt3.Close()
	res3, err := d.conn.Execute(stmt3, nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer res3.Close()
	if res3.HasNext() {
		t, err := res3.Next()
		if err == nil && t != nil {
			m, err := t.GetAsMap()
			if err == nil {
				interfaceCount = mapInt64(m, "cnt")
			}
			t.Close()
		}
	}

	stmt4, err := d.conn.Prepare(`MATCH (c:Class) WHERE c.type_category = 'class' OR c.type_category = 'struct' RETURN count(c) AS cnt`)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer stmt4.Close()
	res4, err := d.conn.Execute(stmt4, nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer res4.Close()
	if res4.HasNext() {
		t, err := res4.Next()
		if err == nil && t != nil {
			m, err := t.GetAsMap()
			if err == nil {
				concreteCount = mapInt64(m, "cnt")
			}
			t.Close()
		}
	}

	return funcCount, classCount, interfaceCount, concreteCount, nil
}

// GetFunctionRange retrieves the file path, start line, and end line for a function by its ID.
func (d *Database) GetFunctionRange(id string) (string, int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	stmt, err := d.conn.Prepare(`MATCH (fn:Function {id: $id}) RETURN fn.file_path, fn.start_line, fn.end_line`)
	if err != nil {
		return "", 0, 0, err
	}
	defer stmt.Close()

	res, err := d.conn.Execute(stmt, map[string]any{"id": id})
	if err != nil {
		return "", 0, 0, err
	}
	defer res.Close()

	if res.HasNext() {
		t, err := res.Next()
		if err == nil && t != nil {
			defer t.Close()
			m, err := t.GetAsMap()
			if err == nil {
				return mapStr(m, "fn.file_path"), mapInt64(m, "fn.start_line"), mapInt64(m, "fn.end_line"), nil
			}
		}
	}

	return "", 0, 0, fmt.Errorf("function with ID %q not found", id)
}

type FileEntity struct {
	Path string
	Hash string
}

type DiffData struct {
	Files       []FileEntity
	Functions   []FunctionEntity
	Classes     []ClassEntity
	Variables   []VariableEntity
	ContainsRel map[string]string // maps child ID to parent class name
	Edges       []VisEdge
}

// GetDiffData returns all database data needed for diffing.
func (d *Database) GetDiffData() (*DiffData, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	data := &DiffData{
		ContainsRel: make(map[string]string),
	}

	// 1. Files
	d.queryRows(`MATCH (f:File) RETURN f.path, f.hash`, func(m map[string]any) {
		data.Files = append(data.Files, FileEntity{
			Path: mapStr(m, "f.path"),
			Hash: mapStr(m, "f.hash"),
		})
	})

	// 2. Functions
	d.queryRows(`MATCH (fn:Function) RETURN fn.id, fn.name, fn.file_path, fn.ast_hash`, func(m map[string]any) {
		data.Functions = append(data.Functions, FunctionEntity{
			ID:       mapStr(m, "fn.id"),
			Name:     mapStr(m, "fn.name"),
			FilePath: mapStr(m, "fn.file_path"),
			ASTHash:  mapStr(m, "fn.ast_hash"),
		})
	})

	// 3. Classes
	d.queryRows(`MATCH (c:Class) RETURN c.id, c.name, c.file_path, c.ast_hash`, func(m map[string]any) {
		data.Classes = append(data.Classes, ClassEntity{
			ID:       mapStr(m, "c.id"),
			Name:     mapStr(m, "c.name"),
			FilePath: mapStr(m, "c.file_path"),
			ASTHash:  mapStr(m, "c.ast_hash"),
		})
	})

	// 4. Variables
	d.queryRows(`MATCH (v:Variable) RETURN v.id, v.name, v.file_path`, func(m map[string]any) {
		data.Variables = append(data.Variables, VariableEntity{
			ID:       mapStr(m, "v.id"),
			Name:     mapStr(m, "v.name"),
			FilePath: mapStr(m, "v.file_path"),
		})
	})

	// 5. CONTAINS relationship from Class to Function/Variable
	d.queryRows(`MATCH (c:Class)-[:CONTAINS]->(e) RETURN c.name, e.id`, func(m map[string]any) {
		cName := mapStr(m, "c.name")
		eID := mapStr(m, "e.id")
		data.ContainsRel[eID] = cName
	})

	// 6. Edges (CALLS, IMPORTS, IMPLEMENTS, CONTAINS)
	edgeQueries := []struct {
		query   string
		fromKey string
		toKey   string
		label   string
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
			data.Edges = append(data.Edges, VisEdge{
				From:  mapStr(m, eq.fromKey),
				To:    mapStr(m, eq.toKey),
				Label: eq.label,
			})
		})
	}

	return data, nil
}


