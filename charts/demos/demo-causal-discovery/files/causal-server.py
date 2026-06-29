#!/usr/bin/env python3
"""
causal-mcp-server: MCP HTTP transport server for causal discovery demo.

Exposes tools for causal hypothesis generation, evidence scoring, and confidence updating.
Tools declare `_meta.ui.resourceUri` for interactive visualization via MCP Apps.

All JSON-RPC 2.0 requests arrive as POST / with Content-Type: application/json.
Responses are returned synchronously (HTTP transport, not SSE).
"""
import json, math, os, time, uuid
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone

PORT = int(os.environ.get("PORT", "8080"))

# ── In-memory state for demo purposes ─────────────────────────────────────────

hypotheses = {}  # hypothesis_id -> hypothesis data
evidence_store = {}  # hypothesis_id -> list of evidence items

# ── Causal Tools ─────────────────────────────────────────────────────────────

def propose_hypothesis(args):
    """Generate causal hypotheses from an observational pattern."""
    pattern = args.get("observational_pattern", "")
    domain = args.get("domain", "general")
    
    hypothesis_id = str(uuid.uuid4())[:8]
    
    # Parse the pattern to identify potential cause-effect relationships
    # Simple heuristic: look for "X affects Y", "X leads to Y", etc.
    nodes = []
    edges = []
    
    # Extract variables from pattern (simple keyword-based extraction)
    words = pattern.lower().split()
    variables = []
    for i, word in enumerate(words):
        if word in ["affects", "causes", "leads", "influences", "results", "increases", "decreases"]:
            if i > 0 and i < len(words) - 1:
                cause = words[i-1].rstrip('.,;:')
                effect = words[i+1].rstrip('.,;:')
                variables.extend([cause, effect])
    
    # Deduplicate and create nodes
    unique_vars = list(dict.fromkeys(variables))
    for v in unique_vars:
        nodes.append({"id": v, "label": v, "type": "variable"})
    
    # If we found variables, create edges
    if len(unique_vars) >= 2:
        edges.append({
            "source": unique_vars[0],
            "target": unique_vars[1],
            "label": "potential causal relationship",
            "confidence": 0.5,
            "evidence_count": 0
        })
    
    # Domain-specific templates
    domain_templates = {
        "epidemiology": [
            {"source": "treatment", "target": "outcome", "label": "treatment effect"},
            {"source": "exposure", "target": "disease", "label": "risk factor"}
        ],
        "economics": [
            {"source": "policy", "target": "outcome", "label": "policy effect"},
            {"source": "price", "target": "demand", "label": "price elasticity"}
        ],
        "neuroscience": [
            {"source": "region_a", "target": "region_b", "label": "effective connectivity"},
            {"source": "stimulation", "target": "activity", "label": "intervention effect"}
        ]
    }
    
    # Add domain template curves if applicable
    if domain in domain_templates and not edges:
        for t in domain_templates[domain]:
            edges.append({**t, "confidence": 0.3, "evidence_count": 0})
    
    hypothesis = {
        "hypothesis_id": hypothesis_id,
        "domain": domain,
        "pattern": pattern,
        "nodes": nodes,
        "edges": edges,
        "confidence": 0.5,
        "temporal": None,
        "confounders": [],
        "created_at": datetime.now(timezone.utc).isoformat()
    }
    
    hypotheses[hypothesis_id] = hypothesis
    
    return {
        "hypothesis_id": hypothesis_id,
        "graph": {
            "nodes": nodes,
            "edges": edges
        },
        "confidence": 0.5,
        "message": f"Generated hypothesis {hypothesis_id} from pattern: {pattern[:100]}..."
    }


def score_evidence(args):
    """Score evidence for a given hypothesis."""
    hypothesis_id = args.get("hypothesis_id", "")
    new_evidence = args.get("evidence", "")
    
    if hypothesis_id not in hypotheses:
        return {"error": f"Unknown hypothesis: {hypothesis_id}"}
    
    hypothesis = hypotheses[hypothesis_id]
    
    # Evidence quality scoring heuristic
    evidence_score = 0.5
    evidence_count = len(evidence_store.get(hypothesis_id, [])) + 1
    
    # Positive indicators increase confidence
    positive_indicators = ["randomized", "controlled", "significant", "p <", "confidence interval"]
    for indicator in positive_indicators:
        if indicator in new_evidence.lower():
            evidence_score += 0.1
    
    # Negative indicators decrease confidence
    negative_indicators = ["confounded", "biased", "correlational", "observational", "small sample"]
    for indicator in negative_indicators:
        if indicator in new_evidence.lower():
            evidence_score -= 0.1
    
    # Update hypothesis confidence
    # Simple running average with decay
    old_conf = hypothesis["confidence"]
    new_conf = old_conf * 0.7 + evidence_score * 0.3
    hypothesis["confidence"] = max(0, min(1, new_conf))
    
    # Store evidence
    if hypothesis_id not in evidence_store:
        evidence_store[hypothesis_id] = []
    evidence_store[hypothesis_id].append({
        "text": new_evidence[:200],
        "score": evidence_score,
        "timestamp": datetime.now(timezone.utc).isoformat()
    })
    
    hypothesis["edges"][0]["evidence_count"] = evidence_count if hypothesis["edges"] else 0
    
    return {
        "hypothesis_id": hypothesis_id,
        "confidence": round(hypothesis["confidence"], 2),
        "evidence_score": round(evidence_score, 2),
        "evidence_count": evidence_count,
        "evidence_items": evidence_store[hypothesis_id][-5:],  # Last 5 items
        "confounders_detected": hypothesis.get("confounders", [])
    }


def update_hypothesis_confidence(args):
    """Update hypothesis confidence using Bayesian updating."""
    hypothesis_id = args.get("hypothesis_id", "")
    likelihood = float(args.get("likelihood", 0.5))  # P(E|H)
    prior = float(args.get("prior", 0.5))  # P(H)
    
    if hypothesis_id not in hypotheses:
        return {"error": f"Unknown hypothesis: {hypothesis_id}"}
    
    # Bayesian update: P(H|E) = P(E|H) * P(H) / P(E)
    # Simplified: assume P(E) ≈ P(E|H) * P(H) + P(E|~H) * P(~H)
    # where P(E|~H) is estimated from likelihood
    
    marginal = likelihood * prior + (1 - likelihood) * (1 - prior)
    if marginal > 0:
        posterior = (likelihood * prior) / marginal
    else:
        posterior = prior
    
    hypothesis = hypotheses[hypothesis_id]
    hypothesis["confidence"] = max(0, min(1, posterior))
    
    return {
        "hypothesis_id": hypothesis_id,
        "prior": round(prior, 2),
        "likelihood": round(likelihood, 2),
        "posterior": round(posterior, 2),
        "confidence": round(hypothesis["confidence"], 3)
    }


def add_temporal_constraint(args):
    """Add temporal constraints to a causal hypothesis."""
    hypothesis_id = args.get("hypothesis_id", "")
    time_lag = float(args.get("time_lag", 1.0))
    decay_rate = float(args.get("decay_rate", 0.1))
    
    if hypothesis_id not in hypotheses:
        return {"error": f"Unknown hypothesis: {hypothesis_id}"}
    
    hypothesis = hypotheses[hypothesis_id]
    hypothesis["temporal"] = {
        "time_lag": time_lag,
        "decay_rate": decay_rate,
        "unit": args.get("unit", "hours")
    }
    
    # Add temporal metadata to edges
    for edge in hypothesis["edges"]:
        edge["temporal"] = {
            "lag": time_lag,
            "decay": decay_rate
        }
    
    return {
        "hypothesis_id": hypothesis_id,
        "temporal": hypothesis["temporal"],
        "graph": {
            "nodes": hypothesis["nodes"],
            "edges": hypothesis["edges"]
        }
    }


# ── HTML Apps ────────────────────────────────────────────────────────────────

def causal_dashboard_html():
    """Self-contained interactive causal discovery dashboard."""
    return '''<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Causal Discovery Microscope</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#0f172a;color:#e2e8f0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;padding:16px;font-size:13px}
h1{font-size:18px;font-weight:600;color:#f8fafc;margin-bottom:2px}
.sub{font-size:12px;color:#94a3b8;margin-bottom:16px}
.graph-area{background:#1e293b;border-radius:8px;border:1px solid #334155;padding:16px;margin-bottom:16px}
#cy{width:100%;height:400px;background:#0f172a;border-radius:4px}
.expert-panel{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin-bottom:16px}
.expert-card{background:#1e293b;border:1px solid #334155;border-radius:8px;padding:12px;text-align:center}
.expert-card.active{border-color:#3b82f6}
.expert-name{font-size:12px;font-weight:600;color:#94a3b8;margin-bottom:6px}
.confidence-bar{width:100%;height:4px;background:#334155;border-radius:2px;overflow:hidden;margin-bottom:6px}
.confidence-fill{height:100%;background:#3b82f6;transition:width 0.3s}
.confidence-text{font-size:11px;color:#64748b}
.evidence-list{max-height:200px;overflow-y:auto}
.evidence-item{background:#0f172a;border:1px solid #334155;border-radius:4px;padding:8px;margin-bottom:6px;font-size:11px}
.waiting{text-align:center;padding:60px;color:#64748b;font-size:14px}
</style>
</head>
<body>
<h1>Causal Discovery Microscope</h1>
<div class="sub" id="subtitle">Waiting for causal analysis...</div>

<div id="waiting" class="waiting">Run the propose_hypothesis tool to generate a causal graph.</div>

<div id="dashboard" style="display:none">
<div class="expert-panel" id="expertPanel">
  <div class="expert-card active" id="card-epi">
    <div class="expert-name">Epidemiology</div>
    <div class="confidence-bar"><div class="confidence-fill" id="bar-epi" style="width:0%"></div></div>
    <div class="confidence-text" id="text-epi">0%</div>
  </div>
  <div class="expert-card" id="card-econ">
    <div class="expert-name">Economics</div>
    <div class="confidence-bar"><div class="confidence-fill" id="bar-econ" style="width:0%"></div></div>
    <div class="confidence-text" id="text-econ">0%</div>
  </div>
  <div class="expert-card" id="card-neuro">
    <div class="expert-name">Neuroscience</div>
    <div class="confidence-bar"><div class="confidence-fill" id="bar-neuro" style="width:0%"></div></div>
    <div class="confidence-text" id="text-neuro">0%</div>
  </div>
</div>

<div class="graph-area">
  <div class="expert-name" style="margin-bottom:8px">Causal Graph</div>
  <div id="cy"></div>
</div>

<div class="expert-name" style="margin-bottom:8px">Evidence Log</div>
<div class="evidence-list" id="evidenceList"></div>
</div>

<script src="https://unpkg.com/cytoscape@3.28.1/dist/cytoscape.min.js"></script>
<script>
(function(){
var DATA=null;
var cy=null;

// Initialize Cytoscape
function initGraph(){
  var eles = [];
  if(DATA && DATA.graph){
    for(var n of DATA.graph.nodes||[]){
      eles.push({data:{id:n.id, label:n.label}});
    }
    for(var e of DATA.graph.edges||[]){
      eles.push({data:{id:e.source+'-'+e.target, source:e.source, target:e.target, label:e.label}});
    }
  }
  if(cy){cy.destroy();}
  cy = cytoscape({
    container: document.getElementById('cy'),
    elements: eles,
    style: [
      {selector: 'node', style: {'background-color': '#3b82f6', 'label': 'data(label)', 'color': '#e2e8f0', 'text-valign': 'bottom', 'font-size': 10}},
      {selector: 'edge', style: {'width': 2, 'line-color': '#64748b', 'target-arrow-color': '#64748b', 'target-arrow-shape': 'triangle', 'label': 'data(label)', 'font-size': 8, 'color': '#94a3b8'}}
    ],
    layout: {name: 'cose', animate: true, animationDuration: 500}
  });
}

// Update expert confidence displays
function updateExperts(confidence, domain){
  var domains = ['epidemiology', 'economics', 'neuroscience'];
  for(var d of domains){
    var pct = domain && domain.toLowerCase().includes(d.slice(0,4)) ? confidence * 100 : Math.random() * 30;
    document.getElementById('bar-'+d.slice(0,4)).style.width = pct + '%';
    document.getElementById('text-'+d.slice(0,4)).textContent = Math.round(pct) + '%';
  }
}

// Update evidence list
function updateEvidence(evidenceItems){
  var el = document.getElementById('evidenceList');
  el.innerHTML = '';
  for(var item of (evidenceItems||[])){
    var div = document.createElement('div');
    div.className = 'evidence-item';
    div.textContent = item.text || item;
    el.appendChild(div);
  }
}

// Listen for postMessage from parent
window.addEventListener("message", function(e){
  if(e.source !== window.parent) return;
  var d = e.data;
  if(!d || d.type !== "mcp-app-result") return;
  
  var result = d.result;
  try{ result = JSON.parse(result); } catch{}
  
  DATA = result;
  
  // Hide waiting, show dashboard
  document.getElementById('waiting').style.display = 'none';
  document.getElementById('dashboard').style.display = 'block';
  document.getElementById('subtitle').textContent = 'Hypothesis: ' + (result.hypothesis_id || 'unknown');
  
  // Update graph
  initGraph();
  
  // Update confidence
  if(result.confidence !== undefined){
    updateExperts(result.confidence, result.domain);
  }
  
  // Update evidence
  if(result.evidence_items){
    updateEvidence(result.evidence_items);
  }
});
})();
</script>
</body>
</html>'''


# ── HTTP Handler ─────────────────────────────────────────────────────────────

TOOL_HANDLERS = {
    "propose-hypothesis": propose_hypothesis,
    "score-evidence": score_evidence,
    "update-hypothesis-confidence": update_hypothesis_confidence,
    "add-temporal-constraint": add_temporal_constraint,
}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        pass  # silence default access log

    def do_OPTIONS(self):
        self.send_response(200)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type")
        self.end_headers()

    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        method = body.get("method", "")
        params = body.get("params", {})
        req_id = body.get("id", 1)

        if method == "initialize":
            response = {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {"protocolVersion": "2024-11-05", "capabilities": {"tools": True, "resources": True}}
            }
        elif method == "tools/list":
            tools = [
                {
                    "name": "propose-hypothesis",
                    "description": "Generate causal hypotheses from an observational pattern",
                    "inputSchema": {"type": "object", "properties": {
                        "observational_pattern": {"type": "string"},
                        "domain": {"type": "string", "default": "general"}
                    }, "required": ["observational_pattern"]},
                    "_meta": {"ui": {"resourceUri": "ui://causal-dashboard"}}
                },
                {
                    "name": "score-evidence",
                    "description": "Score evidence for a given hypothesis",
                    "inputSchema": {"type": "object", "properties": {
                        "hypothesis_id": {"type": "string"},
                        "evidence": {"type": "string"}
                    }, "required": ["hypothesis_id"]}
                },
                {
                    "name": "update-hypothesis-confidence",
                    "description": "Update hypothesis confidence using Bayesian updating",
                    "inputSchema": {"type": "object", "properties": {
                        "hypothesis_id": {"type": "string"},
                        "likelihood": {"type": "number", "default": 0.5},
                        "prior": {"type": "number", "default": 0.5}
                    }, "required": ["hypothesis_id"]}
                },
                {
                    "name": "add-temporal-constraint",
                    "description": "Add temporal constraints to a causal hypothesis",
                    "inputSchema": {"type": "object", "properties": {
                        "hypothesis_id": {"type": "string"},
                        "time_lag": {"type": "number", "default": 1.0},
                        "decay_rate": {"type": "number", "default": 0.1},
                        "unit": {"type": "string", "default": "hours"}
                    }, "required": ["hypothesis_id"]}
                },
            ]
            response = {"jsonrpc": "2.0", "id": req_id, "result": {"tools": tools}}
        elif method == "tools/call":
            tool_name = params.get("name", "")
            tool_args = params.get("arguments", {})
            if tool_name in TOOL_HANDLERS:
                result = TOOL_HANDLERS[tool_name](tool_args)
                response = {"jsonrpc": "2.0", "id": req_id, "result": {"content": [{"type": "text", "text": json.dumps(result)}]}}
            else:
                response = {"jsonrpc": "2.0", "id": req_id, "error": {"code": -32601, "message": f"Unknown tool: {tool_name}"}}
        elif method == "resources/read":
            uri = params.get("uri", "")
            if uri == "ui://causal-dashboard":
                response = {
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "result": {"contents": [{"uri": uri, "mimeType": "text/html", "text": causal_dashboard_html()}]}
                }
            else:
                response = {"jsonrpc": "2.0", "id": req_id, "error": {"code": -32602, "message": f"Unknown resource: {uri}"}}
        else:
            response = {"jsonrpc": "2.0", "id": req_id, "error": {"code": -32601, "message": f"Unknown method: {method}"}}

        payload = json.dumps(response).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    print(f"causal-mcp-server listening on :{PORT}", flush=True)
    HTTPServer(("", PORT), Handler).serve_forever()