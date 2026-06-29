#!/usr/bin/env python3
"""
external-research-mcp-server: Academic database search for causal discovery demo.

Provides tools to search Semantic Scholar, arXiv, and PubMed for relevant research
papers. Phase 2 feature - can be added to the demo later.

All JSON-RPC 2.0 requests arrive as POST / with Content-Type: application/json.
Responses are returned synchronously (HTTP transport, not SSE).
"""
import json, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone

PORT = int(os.environ.get("PORT", "8080"))

# ── NewsAPI.io simulation for demo (since we can't use real API keys) ────────
# In production, this would use the actual Semantic Scholar, arXiv, and PubMed APIs

def search_semantic_scholar(args):
    """Search Semantic Scholar for relevant papers."""
    query = args.get("query", "")
    limit = min(int(args.get("limit", 10)), 50)
    
    # Simulated results for demo
    results = []
    base_papers = [
        {"title": "Causal Inference in Statistics: A Primer", "authors": ["Judea Pearl"], "year": 2016, "citations": 3200, "venue": "Wiley"},
        {"title": "The Book of Why: Causal Discovery Explained", "authors": ["Judea Pearl", "Dana Mackenzie"], "year": 2018, "citations": 2100, "venue": "Basic Books"},
        {"title": "Causal Inference: The Mixtape", "authors": ["Scott Cunningham"], "year": 2023, "citations": 1500, "venue": "Yale University Press"},
        {"title": "Introduction to Causal Inference", "authors": ["Alejandro Schuler", "Mark van der Laan"], "year": 2023, "citations": 800, "venue": "arXiv"},
        {"title": "A Crash Course in Causality", "authors": ["Jason Roy"], "year": 2020, "citations": 650, "venue": "Wiley"},
    ]
    
    for i, paper in enumerate(base_papers[:limit]):
        results.append({
            "paper_id": f"ss-{i}",
            "title": paper["title"],
            "authors": paper["authors"],
            "year": paper["year"],
            "citations": paper["citations"],
            "venue": paper["venue"],
            "url": f"https://arxiv.org/abs/demo-{i}",
            "relevance_score": round(0.9 - i * 0.1, 2)
        })
    
    return {"query": query, "results": results, "total": len(results)}


def search_arxiv(args):
    """Search arXiv for relevant papers."""
    query = args.get("query", "")
    max_results = min(int(args.get("max_results", 10)), 50)
    
    # Simulated results for demo
    categories = ["stat.ML", "cs.LG", "q-bio.QM"]
    results = []
    
    for i in range(max_results):
        results.append({
            "arxiv_id": f"2401.{12345 + i}",
            "title": f"Causal Discovery Methods for {query}" if query else f"Advances in Causal Inference",
            "authors": ["Author A", "Author B"],
            "summary": f"This paper presents novel methods for causal discovery using {query or 'machine learning'}...",
            "categories": [categories[i % 3]],
            "published": "2024-01-15",
            "updated": "2024-01-18",
            "url": f"https://arxiv.org/abs/2401.{12345 + i}",
            "pdf_url": f"https://arxiv.org/pdf/2401.{12345 + i}.pdf"
        })
    
    return {"query": query, "results": results, "total": len(results)}


def search_pubmed(args):
    """Search PubMed for relevant papers."""
    query = args.get("query", "")
    max_results = min(int(args.get("max_results", 10)), 50)
    
    # Simulated results for demo
    journals = ["JAMA", "NEJM", "BMJ", "Lancet"]
    results = []
    
    for i in range(max_results):
        results.append({
            "pmid": f"38{i:07d}",
            "title": f"Causal Relationships in {query}" if query else "Causal Inference in Medical Research",
            "authors": ["Smith J", "Jones M", "Lee K"],
            "journal": journals[i % 4],
            "year": 2023,
            "doi": f"10.1001/jama.2023.{i}",
            "url": f"https://pubmed.ncbi.nlm.nih.gov/38{i:07d}",
            "abstract": f"Background: Understanding causal relationships is critical for... Methods: We used causal inference methods to analyze... Results: We found significant evidence for... Conclusions: These findings suggest..."
        })
    
    return {"query": query, "results": results, "total": len(results)}


def evaluate_evidence_quality(args):
    """Evaluate evidence quality based on metadata."""
    metadata = args.get("metadata", {})
    
    score = 0.5  # Base score
    
    # Positive factors
    if metadata.get("journal") in ["JAMA", "NEJM", "Nature", "Science"]:
        score += 0.3
    if metadata.get("citations", 0) > 1000:
        score += 0.2
    if metadata.get("year", 0) >= 2020:
        score += 0.1
    if metadata.get("has_pvalue", False):
        score += 0.1
    
    # Negative factors
    if metadata.get("is_preprint", False):
        score -= 0.2
    if metadata.get("sample_size", 1000) < 100:
        score -= 0.15
    
    return {
        "quality_score": round(max(0, min(1, score)), 2),
        "factors": {
            "journal_impact": metadata.get("journal") in ["JAMA", "NEJM"],
            "high_citations": metadata.get("citations", 0) > 1000,
            "recent": metadata.get("year", 0) >= 2020,
            "preprint": metadata.get("is_preprint", False)
        }
    }


# ── HTTP Handler ─────────────────────────────────────────────────────────────

TOOL_HANDLERS = {
    "search-semantic-scholar": search_semantic_scholar,
    "search-arxiv": search_arxiv,
    "search-pubmed": search_pubmed,
    "evaluate-evidence-quality": evaluate_evidence_quality,
}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        pass

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
                "result": {"protocolVersion": "2024-11-05", "capabilities": {"tools": True}}
            }
        elif method == "tools/list":
            tools = [
                {
                    "name": "search-semantic-scholar",
                    "description": "Search Semantic Scholar for causal inference papers",
                    "inputSchema": {"type": "object", "properties": {
                        "query": {"type": "string"},
                        "limit": {"type": "integer", "default": 10}
                    }, "required": ["query"]}
                },
                {
                    "name": "search-arxiv",
                    "description": "Search arXiv for preprints on causal discovery",
                    "inputSchema": {"type": "object", "properties": {
                        "query": {"type": "string"},
                        "max_results": {"type": "integer", "default": 10}
                    }, "required": ["query"]}
                },
                {
                    "name": "search-pubmed",
                    "description": "Search PubMed for medical causal studies",
                    "inputSchema": {"type": "object", "properties": {
                        "query": {"type": "string"},
                        "max_results": {"type": "integer", "default": 10}
                    }, "required": ["query"]}
                },
                {
                    "name": "evaluate-evidence-quality",
                    "description": "Score evidence quality based on metadata",
                    "inputSchema": {"type": "object", "properties": {
                        "metadata": {"type": "object"}
                    }, "required": ["metadata"]}
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
        else:
            response = {"jsonrpc": "2.0", "id": req_id, "error": {"code": -32601, "message": f"Unknown method: {method}"}}

        payload = json.dumps(response).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    print(f"external-research-mcp-server listening on :{PORT}", flush=True)
    HTTPServer(("", PORT), Handler).serve_forever()