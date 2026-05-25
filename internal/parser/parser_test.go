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

// Greet is a exported function that says hello.
func Greet(name string) string {
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
	}
}

type sitterFn struct {
	name     string
	doc      string
	exported bool
}
