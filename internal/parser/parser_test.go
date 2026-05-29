package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFile_Go(t *testing.T) {
	// Create a temp directory
	tmpDir, err := os.MkdirTemp("", "contextshrinker-parser-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	content := `package test
import "fmt"
import (
	"os"
)

// Greet is a exported function that says hello.
func Greet(name string) string {
	fmt.Println("logging")
	return "Hello " + name
}

// User is a simple struct.
type User struct {
	Name string
	Age  int
}

func (u *User) GetAge() int {
	return u.Age
}
`
	filePath := filepath.Join(tmpDir, "test.go")
	err = os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	res, err := ParseFile(filePath)
	if err != nil {
		t.Fatalf("failed to parse file: %v", err)
	}

	if len(res.Functions) != 2 {
		t.Errorf("expected 2 functions, got %d", len(res.Functions))
	}

	// Verify ASTHash on functions
	for _, fn := range res.Functions {
		if fn.ASTHash == "" {
			t.Errorf("expected function %s to have ASTHash, but it was empty", fn.Name)
		}
	}

	// Verify Greet
	var greetFn *sitterFn
	for _, fn := range res.Functions {
		if fn.Name == "Greet" {
			greetFn = &sitterFn{fn.Name, fn.Docstring, fn.IsExported}
		}
	}

	if greetFn == nil {
		t.Error("expected to find Greet function")
	} else {
		if greetFn.doc != "Greet is a exported function that says hello." {
			t.Errorf("expected doc %q, got %q", "Greet is a exported function that says hello.", greetFn.doc)
		}
		if !greetFn.exported {
			t.Error("expected Greet to be exported")
		}
	}

	// Verify User struct
	if len(res.Classes) != 1 {
		t.Errorf("expected 1 class/struct, got %d", len(res.Classes))
	} else {
		c := res.Classes[0]
		if c.Name != "User" {
			t.Errorf("expected class name 'User', got %q", c.Name)
		}
		if c.TypeCategory != "struct" {
			t.Errorf("expected category 'struct', got %q", c.TypeCategory)
		}
		if c.ASTHash == "" {
			t.Error("expected Class User to have ASTHash, but it was empty")
		}
	}

	// Verify Imports
	if len(res.Imports) != 2 {
		t.Errorf("expected 2 imports, got %d: %+v", len(res.Imports), res.Imports)
	} else {
		if res.Imports[0].Path != "fmt" {
			t.Errorf("expected first import to be 'fmt', got %q", res.Imports[0].Path)
		}
		if res.Imports[1].Path != "os" {
			t.Errorf("expected second import to be 'os', got %q", res.Imports[1].Path)
		}
	}
}

type sitterFn struct {
	name     string
	doc      string
	exported bool
}

func TestParseFile_Go_DuplicateNamesSameLine(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "contextshrinker-parser-test-dup")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// In Go: `var B struct { B string }` declares a variable B and a struct field B on the same line
	content := `package test
var B struct { B string }
`
	filePath := filepath.Join(tmpDir, "test.go")
	err = os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	res, err := ParseFile(filePath)
	if err != nil {
		t.Fatalf("failed to parse file: %v", err)
	}

	if len(res.Variables) != 2 {
		t.Fatalf("expected 2 variables, got %d: %+v", len(res.Variables), res.Variables)
	}

	v1 := res.Variables[0]
	v2 := res.Variables[1]

	if v1.Name != "B" || v2.Name != "B" {
		t.Errorf("expected both variables to have name 'B', got %q and %q", v1.Name, v2.Name)
	}

	if v1.ID == v2.ID {
		t.Errorf("expected variables on same line to have different IDs, but both got %q", v1.ID)
	}
}
