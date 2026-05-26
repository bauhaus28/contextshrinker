package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type LSPClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	reqID   int64
	pending map[int64]chan *Response
	mu      sync.Mutex
	closed  bool
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

type ResponseError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type Position struct {
	Line      int64 `json:"line"`      // 0-indexed
	Character int64 `json:"character"` // 0-indexed
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// FindLSPBinary resolves the path to an LSP executable.
func FindLSPBinary(lang string) (string, error) {
	var names []string
	switch lang {
	case "go":
		names = []string{"gopls"}
	case "python":
		names = []string{"pyright", "basedpyright"}
	case "javascript", "typescript":
		names = []string{"typescript-language-server", "tsserver", "vtsls"}
	case "java":
		names = []string{"jdtls"}
	default:
		return "", fmt.Errorf("no known LSP binary for language: %s", lang)
	}

	// 1. Check system PATH first
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}

	// 2. Check sandboxed local directory (~/.local/share/contextshrinker/bin)
	homeDir, err := os.UserHomeDir()
	if err == nil {
		sandboxDir := filepath.Join(homeDir, ".local", "share", "contextshrinker", "bin")
		for _, name := range names {
			path := filepath.Join(sandboxDir, name)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}

	return "", fmt.Errorf("LSP binary not found for language %s (checked: %s)", lang, strings.Join(names, ", "))
}

// NewLSPClient spawns the LSP binary and connects to standard IO.
func NewLSPClient(binaryPath string, args []string) (*LSPClient, error) {
	cmd := exec.Command(binaryPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}

	client := &LSPClient{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		pending: make(map[int64]chan *Response),
	}

	go client.readLoop()

	return client, nil
}

func (c *LSPClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	c.stdin.Close()
	c.stdout.Close()
	_ = c.cmd.Process.Kill()
}

func (c *LSPClient) readLoop() {
	reader := bufio.NewReader(c.stdout)
	for {
		buf, err := readMessage(reader)
		if err != nil {
			c.Close()
			return
		}

		var resp Response
		if err := json.Unmarshal(buf, &resp); err != nil {
			continue
		}

		if resp.ID != nil {
			c.mu.Lock()
			ch, exists := c.pending[*resp.ID]
			if exists {
				delete(c.pending, *resp.ID)
				ch <- &resp
			}
			c.mu.Unlock()
		}
	}
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &contentLength)
			}
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("invalid content-length")
	}
	buf := make([]byte, contentLength)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func (c *LSPClient) sendRequest(ctx context.Context, method string, params any) (*Response, error) {
	id := atomic.AddInt64(&c.reqID, 1)
	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	ch := make(chan *Response, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	data, err := json.Marshal(req)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.mu.Lock()
	_, err1 := io.WriteString(c.stdin, header)
	_, err2 := c.stdin.Write(data)
	c.mu.Unlock()

	if err1 != nil || err2 != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to write to LSP process")
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("LSP error (code %d): %s", resp.Error.Code, resp.Error.Message)
		}
		return resp, nil
	}
}

func (c *LSPClient) sendNotification(method string, params any) error {
	notif := Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(notif)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("client closed")
	}
	_, err1 := io.WriteString(c.stdin, header)
	_, err2 := c.stdin.Write(data)
	if err1 != nil {
		return err1
	}
	return err2
}

func (c *LSPClient) Initialize(ctx context.Context, workspacePath string) error {
	absPath, err := filepath.Abs(workspacePath)
	if err != nil {
		absPath = workspacePath
	}
	uri := PathToURI(absPath)

	params := map[string]any{
		"processId": os.Getpid(),
		"rootPath":    absPath,
		"rootUri":     uri,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"references": map[string]any{
					"dynamicRegistration": false,
				},
			},
		},
	}

	_, err = c.sendRequest(ctx, "initialize", params)
	if err != nil {
		return err
	}

	_ = c.sendNotification("initialized", map[string]any{})
	return nil
}

func (c *LSPClient) DidOpen(filePath, text string) error {
	uri := PathToURI(filePath)
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": detectLanguageID(filePath),
			"version":    1,
			"text":       text,
		},
	}
	return c.sendNotification("textDocument/didOpen", params)
}

func (c *LSPClient) References(ctx context.Context, filePath string, line, character int64) ([]Location, error) {
	uri := PathToURI(filePath)
	params := map[string]any{
		"textDocument": map[string]any{
			"uri": uri,
		},
		"position": Position{
			Line:      line,
			Character: character,
		},
		"context": map[string]any{
			"includeDeclaration": true,
		},
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	resp, err := c.sendRequest(ctx, "textDocument/references", params)
	if err != nil {
		return nil, err
	}

	var locations []Location
	if len(resp.Result) > 0 && string(resp.Result) != "null" {
		if err := json.Unmarshal(resp.Result, &locations); err != nil {
			// Single location instead of array is possible in some older LSPs
			var single Location
			if err := json.Unmarshal(resp.Result, &single); err == nil {
				locations = []Location{single}
			} else {
				return nil, err
			}
		}
	}

	return locations, nil
}

func detectLanguageID(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".java":
		return "java"
	default:
		return "plaintext"
	}
}

func PathToURI(path string) string {
	u := url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
	}
	return u.String()
}

func URIToPath(uriStr string) (string, error) {
	u, err := url.Parse(uriStr)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("unsupported URI scheme: %s", u.Scheme)
	}
	return filepath.FromSlash(u.Path), nil
}

// LSPManager wraps all active LSPs for different languages.
type LSPManager struct {
	workspaceRoot string
	clients       map[string]*LSPClient
	mu            sync.Mutex
}

func NewLSPManager(workspaceRoot string) *LSPManager {
	return &LSPManager{
		workspaceRoot: workspaceRoot,
		clients:       make(map[string]*LSPClient),
	}
}

func (m *LSPManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, client := range m.clients {
		client.Close()
	}
}

// GetClient retrieves or spawns the LSP client for the given language.
func (m *LSPManager) GetClient(lang string) (*LSPClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if client, exists := m.clients[lang]; exists {
		return client, nil
	}

	binaryPath, err := FindLSPBinary(lang)
	if err != nil {
		// Attempt auto-install
		binaryPath, err = AutoInstallLSP(lang)
		if err != nil {
			return nil, fmt.Errorf("failed to locate or install LSP for %s: %w", lang, err)
		}
	}

	var args []string
	if lang == "go" {
		args = []string{"-mode=stdio"}
	}

	log.Printf("Starting LSP for %s using binary %s", lang, binaryPath)
	client, err := NewLSPClient(binaryPath, args)
	if err != nil {
		return nil, fmt.Errorf("failed to start LSP for %s: %w", lang, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Initialize(ctx, m.workspaceRoot); err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to initialize LSP for %s: %w", lang, err)
	}

	m.clients[lang] = client
	return client, nil
}

func AutoInstallLSP(lang string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sandboxDir := filepath.Join(homeDir, ".local", "share", "contextshrinker")
	binDir := filepath.Join(sandboxDir, "bin")
	_ = os.MkdirAll(binDir, 0755)

	log.Printf("LSP binary for %s is missing. Attempting to install programmatically...", lang)

	switch lang {
	case "go":
		log.Println("Running: go install golang.org/x/tools/gopls@latest ...")
		cmd := exec.Command("go", "install", "golang.org/x/tools/gopls@latest")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Printf("Failed to run 'go install': %v", err)
			printGoInstallInstructions()
			return "", err
		}
		gopath := os.Getenv("GOPATH")
		if gopath == "" {
			gopath = filepath.Join(homeDir, "go")
		}
		path := filepath.Join(gopath, "bin", "gopls")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		if p, err := exec.LookPath("gopls"); err == nil {
			return p, nil
		}

	case "python":
		log.Printf("Running: npm install --prefix %s pyright ...", sandboxDir)
		cmd := exec.Command("npm", "install", "--prefix", sandboxDir, "pyright")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Printf("Failed to run 'npm install': %v", err)
			printNPMInstallInstructions("python", "pyright")
			return "", err
		}
		path := filepath.Join(sandboxDir, "node_modules", ".bin", "pyright")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}

	case "javascript", "typescript":
		log.Printf("Running: npm install --prefix %s typescript-language-server typescript ...", sandboxDir)
		cmd := exec.Command("npm", "install", "--prefix", sandboxDir, "typescript-language-server", "typescript")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Printf("Failed to run 'npm install': %v", err)
			printNPMInstallInstructions("JS/TS", "typescript-language-server")
			return "", err
		}
		path := filepath.Join(sandboxDir, "node_modules", ".bin", "typescript-language-server")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}

	case "java":
		printJavaInstallInstructions()
		return "", fmt.Errorf("manual installation of Eclipse jdtls is required")
	}

	return "", fmt.Errorf("failed to auto-install LSP for language: %s", lang)
}

func printGoInstallInstructions() {
	log.Println("=========================================================================")
	log.Println("MANUAL INSTALLATION REQUIRED FOR GO LSP (gopls)")
	log.Println("1. Make sure you have Go installed on your system.")
	log.Println("2. Run the following command in your terminal:")
	log.Println("   go install golang.org/x/tools/gopls@latest")
	log.Println("3. Ensure that your $GOPATH/bin (usually ~/go/bin) is in your system PATH.")
	log.Println("=========================================================================")
}

func printNPMInstallInstructions(lang, name string) {
	log.Printf("=========================================================================")
	log.Printf("MANUAL INSTALLATION REQUIRED FOR %s LSP (%s)", strings.ToUpper(lang), name)
	log.Println("1. Make sure you have Node.js and npm installed on your system.")
	log.Printf("2. Run the following command to install it globally:")
	log.Printf("   npm install -g %s", name)
	log.Println("=========================================================================")
}

func printJavaInstallInstructions() {
	log.Println("=========================================================================")
	log.Println("MANUAL INSTALLATION REQUIRED FOR JAVA LSP (jdtls)")
	log.Println("1. Download the Eclipse JDT Language Server tarball from:")
	log.Println("   https://download.eclipse.org/jdtls/milestones/")
	log.Println("2. Extract the archive into ~/.local/share/contextshrinker/jdtls/")
	log.Println("3. Create a wrapper script or symlink named 'jdtls' in your PATH pointing to the executable.")
	log.Println("=========================================================================")
}

