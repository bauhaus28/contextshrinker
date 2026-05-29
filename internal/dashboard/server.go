package dashboard

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bauhaus28/contextshrinker/internal/db"
	"github.com/bauhaus28/contextshrinker/internal/ignore"
	"github.com/bauhaus28/contextshrinker/internal/indexer"
	"github.com/bauhaus28/contextshrinker/internal/report"
)

//go:embed index.html
var indexHTML []byte

type Server struct {
	workspaceRoot string
	database      *db.Database
	ignoreList    *ignore.IgnoreList
}

func StartServer(workspaceRoot string, database *db.Database, ignoreList *ignore.IgnoreList, port int) error {
	s := &Server{
		workspaceRoot: workspaceRoot,
		database:      database,
		ignoreList:    ignoreList,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/graph", s.handleGraph)
	mux.HandleFunc("/api/diff", s.handleDiff)
	mux.HandleFunc("/api/explain", s.handleExplain)

	serverAddr := fmt.Sprintf("localhost:%d", port)
	log.Printf("Starting ContextShrinker Dashboard on http://%s", serverAddr)
	return http.ListenAndServe(serverAddr, mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	nodes, edges, err := s.database.ExportGraph()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to export graph: %v", err), http.StatusInternalServerError)
		return
	}

	rules, _ := report.LoadBoundaryRules(s.workspaceRoot)
	aiMetrics, err := report.CalculateAIMetrics(s.database, rules)
	if err != nil {
		log.Printf("Failed to calculate AI metrics: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"nodes":   nodes,
		"edges":   edges,
		"metrics": aiMetrics,
	})
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		http.Error(w, "Missing 'ref' query parameter", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	diff, err := indexer.DiffGraph(ctx, s.workspaceRoot, s.database, s.ignoreList, ref)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to calculate graph diff: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(diff)
}

type ExplainRequest struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	NodeType string `json:"node_type"`
	FilePath string `json:"file_path"`
}

func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ExplainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	var codeSnippet string
	if req.NodeType == "Function" {
		filePath, start, end, err := s.database.GetFunctionRange(req.NodeID)
		if err == nil {
			snippet, readErr := readCodeLines(filePath, start, end)
			if readErr == nil {
				codeSnippet = snippet
			}
		}
	}

	var prompt string
	if codeSnippet != "" {
		prompt = fmt.Sprintf("You are a Principal Software Architect. Review the following code for a %s named '%s' from file '%s'. Explain its purpose and suggest concrete refactoring or optimization steps to improve modularity, simplify complexity, avoid duplication, and ensure architectural boundaries are respected. Present your review using clean Markdown with distinct headers.\n\nCode:\n```go\n%s\n```\n", req.NodeType, req.NodeName, req.FilePath, codeSnippet)
	} else {
		prompt = fmt.Sprintf("You are a Principal Software Architect. Review the structure of the %s named '%s' from file '%s'. Explain its architectural role in the codebase and recommend best-practice structural enhancements or modularity optimizations. Present your review using clean Markdown with distinct headers.", req.NodeType, req.NodeName, req.FilePath)
	}

	explanation, err := callLLM(prompt)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"explanation": explanation,
	})
}

func readCodeLines(filePath string, startLine, endLine int64) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	var currentLine int64 = 1
	for scanner.Scan() {
		if currentLine >= startLine && currentLine <= endLine {
			lines = append(lines, scanner.Text())
		}
		if currentLine > endLine {
			break
		}
		currentLine++
	}
	return strings.Join(lines, "\n"), scanner.Err()
}

func callLLM(prompt string) (string, error) {
	geminiKey := os.Getenv("GEMINI_API_KEY")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")

	if geminiKey != "" {
		return callGemini(geminiKey, prompt)
	}
	if anthropicKey != "" {
		return callAnthropic(anthropicKey, prompt)
	}
	if openaiKey != "" {
		return callOpenAI(openaiKey, prompt)
	}

	return "", fmt.Errorf("No API keys found in the environment. Please set GEMINI_API_KEY, ANTHROPIC_API_KEY, or OPENAI_API_KEY.")
}

func callGemini(apiKey, prompt string) (string, error) {
	url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=" + apiKey

	reqBody, _ := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{
				"parts": []any{
					map[string]any{
						"text": prompt,
					},
				},
			},
		},
	})

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Gemini API error (status %d): %s", resp.StatusCode, string(body))
	}

	var res struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	if len(res.Candidates) > 0 && len(res.Candidates[0].Content.Parts) > 0 {
		return res.Candidates[0].Content.Parts[0].Text, nil
	}

	return "", fmt.Errorf("empty response from Gemini API")
}

func callAnthropic(apiKey, prompt string) (string, error) {
	url := "https://api.anthropic.com/v1/messages"

	reqBody, _ := json.Marshal(map[string]any{
		"model":      "claude-3-5-sonnet-latest",
		"max_tokens": 1024,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": prompt,
			},
		},
	})

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Anthropic API error (status %d): %s", resp.StatusCode, string(body))
	}

	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	if len(res.Content) > 0 {
		return res.Content[0].Text, nil
	}

	return "", fmt.Errorf("empty response from Anthropic API")
}

func callOpenAI(apiKey, prompt string) (string, error) {
	url := "https://api.openai.com/v1/chat/completions"

	reqBody, _ := json.Marshal(map[string]any{
		"model": "gpt-4o-mini",
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": prompt,
			},
		},
	})

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OpenAI API error (status %d): %s", resp.StatusCode, string(body))
	}

	var res struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	if len(res.Choices) > 0 {
		return res.Choices[0].Message.Content, nil
	}

	return "", fmt.Errorf("empty response from OpenAI API")
}
