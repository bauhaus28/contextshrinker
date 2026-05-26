package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bauhaus28/contextshrinker/internal/db"
)

// GenerateGraphHTML creates a self-contained, interactive HTML visualization of the codebase graph.
func GenerateGraphHTML(workspaceRoot string, nodes []db.VisNode, edges []db.VisEdge) (string, error) {
	nodesJSON, err := json.Marshal(nodes)
	if err != nil {
		return "", err
	}

	edgesJSON, err := json.Marshal(edges)
	if err != nil {
		return "", err
	}

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>contextshrinker Codebase Graph</title>
    <!-- vis-network 9.1.9 pinned. Compute SRI hash with: curl -s URL | openssl dgst -sha384 -binary | openssl base64 -A -->
    <script type="text/javascript" src="https://unpkg.com/vis-network@9.1.9/standalone/umd/vis-network.min.js" crossorigin="anonymous"></script>
    <style>
        body {
            background-color: #0f172a;
            color: #f8fafc;
            font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            margin: 0;
            padding: 0;
            display: flex;
            height: 100vh;
            overflow: hidden;
        }
        #sidebar {
            width: 320px;
            background: rgba(30, 41, 59, 0.75);
            backdrop-filter: blur(12px);
            border-right: 1px solid rgba(255, 255, 255, 0.1);
            padding: 24px;
            display: flex;
            flex-direction: column;
            gap: 20px;
            box-sizing: border-box;
            z-index: 10;
            overflow-y: auto;
        }
        #network-container {
            flex: 1;
            position: relative;
            height: 100%%;
        }
        #network {
            width: 100%%;
            height: 100%%;
        }
        h1 {
            font-size: 22px;
            margin: 0;
            background: linear-gradient(to right, #60a5fa, #a78bfa);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            font-weight: 800;
            letter-spacing: -0.025em;
        }
        .subtitle {
            font-size: 12px;
            color: #94a3b8;
            margin-top: -15px;
        }
        .section-title {
            font-size: 11px;
            text-transform: uppercase;
            letter-spacing: 0.05em;
            color: #94a3b8;
            margin-bottom: 8px;
            font-weight: 700;
        }
        .card {
            background: rgba(15, 23, 42, 0.4);
            border: 1px solid rgba(255, 255, 255, 0.05);
            padding: 16px;
            border-radius: 8px;
        }
        input, select, button {
            background: #1e293b;
            border: 1px solid rgba(255, 255, 255, 0.1);
            color: #f8fafc;
            padding: 8px 12px;
            border-radius: 6px;
            width: 100%%;
            box-sizing: border-box;
            outline: none;
            transition: all 0.2s;
            font-size: 13px;
        }
        input:focus {
            border-color: #60a5fa;
            box-shadow: 0 0 0 2px rgba(96, 165, 250, 0.2);
        }
        button {
            cursor: pointer;
            font-weight: 600;
            background: linear-gradient(135deg, #3b82f6, #8b5cf6);
            border: none;
            color: white;
            padding: 10px;
        }
        button:hover {
            opacity: 0.95;
            transform: translateY(-1px);
        }
        #details-card pre {
            margin: 0;
            white-space: pre-wrap;
            font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
            font-size: 12px;
            color: #cbd5e1;
            max-height: 200px;
            overflow-y: auto;
        }
        .legend-item {
            display: flex;
            align-items: center;
            gap: 10px;
            font-size: 13px;
            margin-bottom: 8px;
        }
        .legend-color {
            width: 12px;
            height: 12px;
            border-radius: 4px;
        }
        #stats {
            font-size: 12px;
            color: #94a3b8;
            margin-top: auto;
            border-top: 1px solid rgba(255, 255, 255, 0.1);
            padding-top: 16px;
        }
    </style>
</head>
<body>
    <div id="sidebar">
        <div>
            <h1>contextshrinker</h1>
            <p class="subtitle">Codebase Graph Visualizer</p>
        </div>

        <div class="card">
            <div class="section-title">Search</div>
            <input type="text" id="search" placeholder="Type entity name..." oninput="searchNode()">
        </div>

        <div class="card">
            <div class="section-title">Legend</div>
            <div class="legend-item"><div class="legend-color" style="background-color: #8b5cf6;"></div>File</div>
            <div class="legend-item"><div class="legend-color" style="background-color: #3b82f6;"></div>Class / Struct</div>
            <div class="legend-item"><div class="legend-color" style="background-color: #10b981;"></div>Function / Method</div>
            <div class="legend-item"><div class="legend-color" style="background-color: #f59e0b;"></div>Variable</div>
        </div>

        <div id="details-card" class="card" style="display:none;">
            <div class="section-title" id="details-type">Details</div>
            <h3 style="margin-top: 0; margin-bottom: 8px;" id="details-name">Name</h3>
            <pre id="details-content"></pre>
        </div>

        <div class="card">
            <button onclick="togglePhysics()">Toggle Physics</button>
        </div>

        <div id="stats">
            Nodes: <span id="node-count">0</span> | Edges: <span id="edge-count">0</span>
        </div>
    </div>

    <div id="network-container">
        <div id="network"></div>
    </div>

    <script type="text/javascript">
        const rawNodes = %s;
        const rawEdges = %s;

        document.getElementById('node-count').innerText = rawNodes.length;
        document.getElementById('edge-count').innerText = rawEdges.length;

        function escapeHtml(str) {
            if (!str) return '';
            return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
                      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
        }

        // Color theme mappings
        const groupColors = {
            'File': { background: '#8b5cf6', border: '#7c3aed', highlight: { background: '#a78bfa', border: '#8b5cf6' } },
            'Class': { background: '#3b82f6', border: '#2563eb', highlight: { background: '#60a5fa', border: '#3b82f6' } },
            'Function': { background: '#10b981', border: '#059669', highlight: { background: '#34d399', border: '#10b981' } },
            'Variable': { background: '#f59e0b', border: '#d97706', highlight: { background: '#fbbf24', border: '#f59e0b' } }
        };

        // Build a lookup map for safe (textContent) display in the details panel.
        const rawNodeMap = {};
        rawNodes.forEach(n => { rawNodeMap[n.id] = n; });

        const nodes = new vis.DataSet(rawNodes.map(n => ({
            id: n.id,
            label: escapeHtml(n.label),
            title: escapeHtml(n.title),  // vis.js renders title as HTML; escape to prevent XSS
            color: groupColors[n.group] || { background: '#94a3b8', border: '#64748b' },
            font: { color: '#f8fafc', size: 14 },
            shape: 'dot',
            size: n.group === 'File' ? 25 : 15,
            group: n.group
        })));

        const edges = new vis.DataSet(rawEdges.map(e => ({
            from: e.from,
            to: e.to,
            label: e.label,
            font: { color: '#64748b', size: 10, align: 'top' },
            arrows: { to: { enabled: true, scaleFactor: 0.5 } },
            color: { color: 'rgba(255, 255, 255, 0.15)', highlight: '#60a5fa' }
        })));

        const container = document.getElementById('network');
        const data = { nodes, edges };
        const options = {
            physics: {
                enabled: true,
                barnesHut: {
                    gravitationalConstant: -2000,
                    centralGravity: 0.3,
                    springLength: 95,
                    springConstant: 0.04,
                    damping: 0.09
                }
            },
            interaction: {
                hover: true,
                tooltipDelay: 200
            }
        };

        const network = new vis.Network(container, data, options);

        network.on("click", function (params) {
            if (params.nodes.length > 0) {
                const nodeId = params.nodes[0];
                const node = nodes.get(nodeId);
                
                const raw = rawNodeMap[nodeId] || {};
                document.getElementById('details-card').style.display = 'block';
                document.getElementById('details-type').textContent = (node.group || '').toUpperCase() + ' DETAILS';
                document.getElementById('details-name').textContent = raw.label || '';
                document.getElementById('details-content').textContent = raw.title || 'No docstring or description';
            } else {
                document.getElementById('details-card').style.display = 'none';
            }
        });

        let physicsEnabled = true;
        function togglePhysics() {
            physicsEnabled = !physicsEnabled;
            network.setOptions({ physics: { enabled: physicsEnabled } });
        }

        function searchNode() {
            const query = document.getElementById('search').value.toLowerCase();
            if (!query) return;

            const found = rawNodes.find(n => n.label.toLowerCase().includes(query));
            if (found) {
                network.focus(found.id, {
                    scale: 1.2,
                    animation: {
                        duration: 800,
                        easingFunction: 'easeInOutQuad'
                    }
                });
                network.selectNodes([found.id]);
            }
        }
    </script>
</body>
</html>`, string(nodesJSON), string(edgesJSON))

	schwobDir := filepath.Join(workspaceRoot, ".contextshrinker")
	if err := os.MkdirAll(schwobDir, 0755); err != nil {
		return "", err
	}

	outputPath := filepath.Join(schwobDir, "contextshrinker_graph.html")
	if err := os.WriteFile(outputPath, []byte(htmlContent), 0644); err != nil {
		return "", err
	}

	return outputPath, nil
}
