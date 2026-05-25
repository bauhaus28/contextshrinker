Here is the updated project specification document, incorporating the universal code generator compatibility, the new languages, the custom ignore file, and the working title. You can drop this directly into your code generation tool.

---

# Project Specification: ContextShrinker

## 1. Project Overview

**ContextShrinker** is an open-source, zero-dependency Model Context Protocol (MCP) server written in Go. Its primary objective is to drastically reduce LLM token bloat for autonomous AI agents and code generation tools (including Claude Code, Cursor, Aider, and any MCP-compliant client). It achieves this by parsing a local codebase into an embedded graph database (Kùzu). Instead of reading raw files to understand context, AI agents query the graph to map architectural dependencies, call chains, and syntax structures.

The tool runs completely headless, auto-manages its own Language Server Protocol (LSP) instances, and relies exclusively on deterministic compiler truth and Full-Text Search (FTS).

## 2. Tech Stack & Key Libraries

* **Core Language:** Go (designed to be cross-compiled natively for `darwin/arm64` and `linux/amd64` to support local development and server deployment).
* **Graph Database:** `github.com/kuzudb/kuzu-go` (Embedded, runs in-process).
* **Syntax Parsing:** `github.com/smacker/go-tree-sitter` (Fast AST extraction).
* **File Watching:** `github.com/fsnotify/fsnotify` (Live state synchronization).
* **File Filtering:** Custom parser for a `.contextshrinkerignore` file (Pruning 3rd-party libs).
* **Communication:** Official Go MCP SDK (Transport via `stdio`).
* **Supported Target Languages:** Go, Python, JavaScript, TypeScript, Java, and Kotlin.

## 3. Core Architectural Requirements

### A. Universal Agent Compatibility & Headless Execution

The application must require zero external databases or AI services to run. It boots up, creates a `.kuzu` folder locally for the graph, and communicates over standard input/output (`stdio`). This allows any MCP-compatible code generator to instantly attach to the server as a tool without requiring dedicated IDE extensions.

### B. The Local Boundary Firewall (3rd-Party Exclusion)

To prevent graph bloat, the tool must explicitly ignore all third-party libraries and focus solely on proprietary code:

1. **Traversal Filter:** Parse a custom `.contextshrinkerignore` file located in the project root. The tool must hardcode fallback skips for standard dependency folders (`node_modules/`, `vendor/`, `__pycache__/`, `.git/`, `build/`, `target/`) using `filepath.WalkDir` so these are never scanned.
2. **Import Boundary:** When an AST `import` statement is detected, verify it belongs to the local module or workspace (e.g., checking `go.mod` in Go, `build.gradle`/`pom.xml` in Java/Kotlin). External dependencies must be dropped; no nodes or edges should be created for them.

### C. Auto-Managed LSP Daemon

The tool must automatically provision semantic intelligence without user intervention:

1. Detect the repository language(s) upon startup.
2. Check for the corresponding LSP binaries in a sandboxed local directory (e.g., `~/.local/share/contextshrinker/bin`):
* Go: `gopls`
* Python: `pyright` or `basedpyright`
* JS/TS: `ts-server`
* Java: `jdtls` (Eclipse JDT Language Server)
* Kotlin: `kotlin-language-server`


3. If missing, programmatically download/install the necessary binary.
4. Spawn the LSP as a hidden background daemon and connect to its RPC interface via standard I/O to query semantic references.

### D. Two-Pass Ingestion Strategy

1. **Pass 1 (Tree-sitter Syntax):** Rapidly scan the local files to extract entities (Functions, Classes, Interfaces, Variables) and their associated docstrings. Insert these as baseline Nodes in Kùzu.
2. **Pass 2 (LSP Semantics):** For the discovered entities, dispatch targeted `textDocument/references` JSON-RPC requests to the managed LSP instance to determine precise relationships (`CALLS`, `IMPORTS`, `IMPLEMENTS`). Insert these as Edges in Kùzu.

### E. Live Synchronization

Use `fsnotify` to watch the project directory. When a file is saved:

1. Debounce the event (e.g., wait 1-2 seconds after the last save).
2. Identify the file hash.
3. Execute a delta update in Kùzu: DELETE all nodes and edges explicitly owned by that file path, then re-run Pass 1 and Pass 2 *only* for that specific file to re-insert the updated logic.

## 4. Kùzu Database Schema Definition

The embedded graph must unify the structures of OOP-heavy languages (Java/Kotlin) and functional/scripting languages (Go, Python, JS/TS) under a single property graph schema.

**Nodes:**

* `File` (Properties: `path`, `hash`, `last_modified`)
* `Function` / `Method` (Properties: `name`, `file_path`, `start_line`, `end_line`, `docstring`, `is_exported`)
* `Class` / `Struct` / `Interface` (Properties: `name`, `file_path`, `docstring`, `type_category`)
* `Variable` / `Constant` (Properties: `name`, `file_path`, `type_hint`)

**Edges:**

* `CALLS` (From Function/Method -> To Function/Method)
* `IMPORTS` (From File -> To File/Entity)
* `CONTAINS` (From File -> To Function/Class, or Class -> To Method/Variable)
* `IMPLEMENTS` / `EXTENDS` (From Class/Struct -> To Interface/Class)

## 5. MCP Interface (Exposed Tools)

The server must expose the following highly structured tools to the LLM agent, utilizing Kùzu's Full-Text Search and Cypher traversal capabilities:

* **`search_codebase`**:
* *Inputs:* `query` (string)
* *Logic:* Executes a Cypher query using `CONTAINS` against the `name` and `docstring` properties of all nodes. Returns matching entity names and file paths.


* **`get_call_chain`**:
* *Inputs:* `target_function` (string), `depth` (integer, max 5)
* *Logic:* Executes a variable-length path Cypher query to trace all upstream callers of the target function.


* **`get_file_structure`**:
* *Inputs:* `file_path` (string)
* *Logic:* Returns all `CONTAINS` edges for a specific file to give the agent an overview of the file's contents without reading the raw text.



## 6. Execution Flow (For AI Context)

1. The Code Generator (Claude Code, Cursor, etc.) initiates the `contextshrinker` binary via its local MCP configuration.
2. `contextshrinker` boots, runs the `.contextshrinkerignore` File Walker to establish the target perimeter.
3. `contextshrinker` detects the languages and spawns the correct LSP daemon(s).
4. Tree-sitter builds the baseline Kùzu graph (Nodes).
5. `contextshrinker` queries the LSP and builds the relational Kùzu graph (Edges).
6. `contextshrinker` begins listening on `stdio` for MCP JSON-RPC requests from the code generator.
7. The agent asks to trace a bug or explore an architecture; `contextshrinker` queries Kùzu via Cypher and returns the token-efficient JSON response.