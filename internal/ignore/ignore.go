package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// IgnoreList handles checking whether files should be ignored based on default patterns and custom .contextshrinkerignore patterns.
type IgnoreList struct {
	patterns []string
}

// NewIgnoreList creates an IgnoreList. It always appends the standard defaults.
func NewIgnoreList(rootDir string) (*IgnoreList, error) {
	il := &IgnoreList{
		patterns: []string{
			"node_modules",
			"vendor",
			"__pycache__",
			".git",
			"build",
			"target",
			".contextshrinker",
			"contextshrinker_graph.html",
			".venv",
			"venv",
			"env",
			".env",
			"dist",
			"out",
			".vscode",
			".idea",
			".cache",
			"tmp",
			// Go-specific defaults
			"testdata",
			"test_cache",
			"test-cache",
			".test_cache",
			"*.test",
			"*.out",
			"*.coverprofile",
			"profile.cov",
			"go.work",
			"go.work.sum",
		},
	}

	// 1. Resolve ignore file path (check .csignore first, then legacy .contextshrinker/ignore, then legacy .contextshrinkerignore)
	ignoreFilePath := filepath.Join(rootDir, ".csignore")
	if _, err := os.Stat(ignoreFilePath); os.IsNotExist(err) {
		legacyPath1 := filepath.Join(rootDir, ".contextshrinker", "ignore")
		if _, errLegacy1 := os.Stat(legacyPath1); errLegacy1 == nil {
			ignoreFilePath = legacyPath1
		} else {
			legacyPath2 := filepath.Join(rootDir, ".contextshrinkerignore")
			if _, errLegacy2 := os.Stat(legacyPath2); errLegacy2 == nil {
				ignoreFilePath = legacyPath2
			} else {
				// No ignore file exists, return the default ignore list
				return il, nil
			}
		}
	}

	file, err := os.Open(ignoreFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return il, nil
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Standardize separators to forward slashes for matching consistency
		line = filepath.ToSlash(line)
		// Strip leading/trailing slashes for easier matching
		line = strings.Trim(line, "/")
		if line != "" {
			il.patterns = append(il.patterns, line)
		}
	}

	return il, scanner.Err()
}

// ShouldIgnore returns true if the given relative path matches any of the ignore patterns.
func (il *IgnoreList) ShouldIgnore(relPath string) bool {
	// Standardize path to use forward slashes
	relPath = filepath.ToSlash(relPath)
	parts := strings.Split(relPath, "/")

	for _, pattern := range il.patterns {
		// 1. Direct segment match (e.g., if a directory segment matches the pattern like "node_modules")
		for _, part := range parts {
			if part == pattern {
				return true
			}
		}

		// 2. Exact match of the relative path
		if relPath == pattern {
			return true
		}

		// 3. Glob matching (using filepath.Match)
		// We try matching the whole path, or individual segments
		matched, _ := filepath.Match(pattern, relPath)
		if matched {
			return true
		}
		for _, part := range parts {
			matched, _ := filepath.Match(pattern, part)
			if matched {
				return true
			}
		}

		// 4. Prefix match (e.g., pattern is "build" and path is "build/output.txt")
		if strings.HasPrefix(relPath, pattern+"/") {
			return true
		}
	}

	return false
}
