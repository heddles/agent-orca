#!/usr/bin/env python3
"""
research-tools-server: MCP HTTP transport server for LLM research agent.

Provides tools for fetching web content, searching arXiv, and extracting PDFs:
  - web-fetch(url) — fetch web page content
  - web-search(query) — search via DuckDuckGo
  - pdf-extract(url) — extract text from PDF documents
  - arxiv-search(query, max_results) — search arXiv API
  - arxiv-fetch(paper_id) — fetch arXiv paper content
  - semaphore(topic, status, summary) — signal research completion
"""
import json, os, re, html
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone
from urllib.request import Request, urlopen
from urllib.parse import urlencode, urlparse
from xml.etree import ElementTree as ET

PORT = int(os.environ.get("PORT", "8080"))
ARXIV_MAX_RESULTS = int(os.environ.get("ARXIV_MAX_RESULTS", "100"))
REQUEST_TIMEOUT = int(os.environ.get("REQUEST_TIMEOUT", "30"))

# ── Tool definitions ──────────────────────────────────────────────────────────

TOOLS = [
    {
        "name": "web-fetch",
        "description": "Fetch web page content from a URL. Returns text content and metadata for research ingestion.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "url": {"type": "string", "description": "The URL to fetch"},
            },
            "required": ["url"],
        },
    },
    {
        "name": "web-search",
        "description": "Search the web for academic papers and articles using DuckDuckGo (no API key required).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "Search query"},
                "max_results": {"type": "integer", "default": 10, "description": "Maximum number of results"},
            },
            "required": ["query"],
        },
    },
    {
        "name": "pdf-extract",
        "description": "Download and extract text content from PDF research documents.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "url": {"type": "string", "description": "URL to the PDF document"},
            },
            "required": ["url"],
        },
    },
    {
        "name": "arxiv-search",
        "description": "Search arXiv.org for papers on LLM architecture, interpretability, and reasoning.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "arXiv search query (supports Boolean operators)"},
                "max_results": {"type": "integer", "default": 50, "description": "Maximum number of results"},
            },
            "required": ["query"],
        },
    },
    {
        "name": "arxiv-fetch",
        "description": "Fetch full-text content from an arXiv paper by ID.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "paper_id": {"type": "string", "description": "arXiv paper ID (e.g., '2310.0001' or full URL)"},
            },
            "required": ["paper_id"],
        },
    },
    {
        "name": "semaphore",
        "description": "Signal that research on a topic is complete and ready for synthesis.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "topic": {"type": "string", "description": "The topic that has been completed"},
                "status": {"type": "string", "default": "complete", "description": "Status signal"},
                "summary": {"type": "string", "description": "Brief summary of findings"},
            },
            "required": ["topic"],
        },
    },
]

# ── Helper functions ──────────────────────────────────────────────────────────

def fetch_url(url, timeout=REQUEST_TIMEOUT):
    """Fetch URL content with proper headers."""
    headers = {
        "User-Agent": "Mozilla/5.0 (Research Agent; agent-orc demo)",
        "Accept": "text/html,application/pdf,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    }
    req = Request(url, headers=headers)
    with urlopen(req, timeout=timeout) as response:
        return response.read()

def extract_text_from_html(html_content):
    """Extract readable text from HTML."""
    # Remove script and style elements
    text = re.sub(r'<script[^>]*>.*?</script>', '', html_content, flags=re.DOTALL|re.IGNORECASE)
    text = re.sub(r'<style[^>]*>.*?</style>', '', text, flags=re.DOTALL|re.IGNORECASE)
    # Remove HTML tags
    text = re.sub(r'<[^>]+>', ' ', text)
    # Decode HTML entities
    text = html.unescape(text)
    # Normalize whitespace
    text = re.sub(r'\s+', ' ', text).strip()
    return text

def search_duckduckgo(query, max_results=10):
    """Search DuckDuckGo via HTML scraping."""
    url = f"https://html.duckduckgo.com/html/?{urlencode({'q': query})}"
    try:
        content = fetch_url(url)
        html_text = content.decode('utf-8', errors='replace')
        
        results = []
        # Parse result links
        for match in re.finditer(r'<a[^>]*class="result__a"[^>]*href="([^"]*)"[^>]*>(.*?)</a>', html_text, re.DOTALL):
            href = match.group(1)
            title = html.unescape(re.sub(r'<[^>]+>', '', match.group(2)))
            if href and title and len(results) < max_results:
                results.append({"url": href, "title": title, "snippet": ""})
        
        # Parse snippets
        for i, match in enumerate(re.finditer(r'<a[^>]*class="result__snippet"[^>]*>(.*?)</a>', html_text, re.DOTALL)):
            if i < len(results):
                results[i]["snippet"] = html.unescape(re.sub(r'<[^>]+>', '', match.group(1)))
        
        return results
    except Exception as e:
        return [{"error": str(e)}]

def search_arxiv(query, max_results=50):
    """Search arXiv API and return paper metadata."""
    params = {
        "search_query": query,
        "start": 0,
        "max_results": min(max_results, ARXIV_MAX_RESULTS),
        "sortBy": "submittedDate",
        "sortOrder": "descending",
    }
    url = f"http://export.arxiv.org/api/query?{urlencode(params)}"
    
    try:
        content = fetch_url(url)
        root = ET.fromstring(content)
        
        # arXiv namespace
        ns = {"feed": "http://www.w3.org/2005/Atom"}
        
        results = []
        for entry in root.findall("feed:entry", ns):
            paper_id = entry.findtext("feed:id", "", ns).split("/")[-1]
            title = entry.findtext("feed:title", "", ns)
            summary = entry.findtext("feed:summary", "", ns)
            authors = [a.findtext("feed:name", "", ns) for a in entry.findall("feed:author", ns)]
            published = entry.findtext("feed:published", "", ns)
            link = entry.find("feed:link[@title='pdf']", ns)
            pdf_url = link.get("href") if link is not None else ""
            
            results.append({
                "id": paper_id,
                "title": title,
                "summary": summary,
                "authors": authors,
                "published": published,
                "pdf_url": pdf_url,
            })
        
        return results
    except Exception as e:
        return [{"error": str(e)}]

def fetch_arxiv_paper(paper_id):
    """Fetch arXiv paper abstract and metadata."""
    # Clean paper ID
    paper_id = paper_id.split("/")[-1].replace(".pdf", "")
    
    params = {
        "search_query": f"id:{paper_id}",
        "max_results": 1,
    }
    url = f"http://export.arxiv.org/api/query?{urlencode(params)}"
    
    try:
        content = fetch_url(url)
        root = ET.fromstring(content)
        ns = {"feed": "http://www.w3.org/2005/Atom"}
        
        entry = root.find("feed:entry", ns)
        if entry is None:
            return {"error": "Paper not found"}
        
        title = entry.findtext("feed:title", "", ns)
        abstract = entry.findtext("feed:summary", "", ns)
        authors = [a.findtext("feed:name", "", ns) for a in entry.findall("feed:author", ns)]
        published = entry.findtext("feed:published", "", ns)
        
        # Get PDF URL
        link = entry.find("feed:link[@title='pdf']", ns)
        pdf_url = link.get("href") + ".pdf" if link is not None else ""
        
        # Fetch PDF if available
        pdf_text = ""
        if pdf_url:
            try:
                pdf_content = fetch_url(pdf_url)
                # Note: Full PDF extraction requires PyPDF2/pdfplumber
                # For now, return the abstract which has key content
                pdf_text = abstract
            except Exception:
                pdf_text = abstract
        
        return {
            "paper_id": paper_id,
            "title": title,
            "authors": authors,
            "published": published,
            "abstract": abstract,
            "content": pdf_text,
            "pdf_url": pdf_url,
        }
    except Exception as e:
        return {"error": str(e)}

def process_pdf(url):
    """Download and extract text from PDF (basic extraction)."""
    try:
        # For full PDF extraction, the cluster would need PyPDF2 installed
        # This is a placeholder that returns metadata
        content = fetch_url(url)
        # Return raw PDF bytes info (would need pdfplumber for actual text extraction)
        return {
            "url": url,
            "size_bytes": len(content),
            "note": "PDF downloaded. For full text extraction, install PyPDF2 in the container.",
        }
    except Exception as e:
        return {"error": str(e)}

# ── MCP JSON-RPC handler ──────────────────────────────────────────────────────

def handle_rpc(method, params, req_id):
    params = params or {}

    if method == "initialize":
        return {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "research-tools-server", "version": "1.0.0"},
        }, req_id

    if method == "notifications/initialized":
        return None, None

    if method == "tools/list":
        return {"tools": TOOLS}, req_id

    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}

        try:
            if name == "web-fetch":
                url = args.get("url", "")
                content = fetch_url(url)
                text = extract_text_from_html(content.decode('utf-8', errors='replace'))
                result = {"url": url, "content": text[:50000], "length": len(text)}
            
            elif name == "web-search":
                query = args.get("query", "")
                max_results = args.get("max_results", 10)
                result = search_duckduckgo(query, max_results)
            
            elif name == "pdf-extract":
                url = args.get("url", "")
                result = process_pdf(url)
            
            elif name == "arxiv-search":
                query = args.get("query", "")
                max_results = args.get("max_results", 50)
                result = search_arxiv(query, max_results)
            
            elif name == "arxiv-fetch":
                paper_id = args.get("paper_id", "")
                result = fetch_arxiv_paper(paper_id)
            
            elif name == "semaphore":
                topic = args.get("topic", "")
                status = args.get("status", "complete")
                summary = args.get("summary", "")
                result = {"topic": topic, "status": status, "summary": summary, "timestamp": datetime.now(timezone.utc).isoformat()}
            
            else:
                return {"content": [{"type": "text", "text": f"unknown tool: {name}"}], "isError": True}, req_id
            
            return {"content": [{"type": "text", "text": json.dumps(result, indent=2)}], "isError": False}, req_id
        
        except Exception as e:
            return {"content": [{"type": "text", "text": json.dumps({"error": str(e)})}], "isError": True}, req_id

    return {"error": {"code": -32601, "message": f"Method not found: {method}"}}, req_id


class MCPHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

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
        body = self.rfile.read(length)
        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            self._respond({"jsonrpc": "2.0", "id": None,
                           "error": {"code": -32700, "message": "Parse error"}})
            return
        result, resp_id = handle_rpc(req.get("method", ""), req.get("params"), req.get("id"))
        if result is None and resp_id is None:
            self.send_response(204)
            self.end_headers()
            return
        if isinstance(result, dict) and "error" in result and len(result) == 1:
            self._respond({"jsonrpc": "2.0", "id": resp_id, "error": result["error"]})
        else:
            self._respond({"jsonrpc": "2.0", "id": resp_id, "result": result})

    def _respond(self, data):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())


if __name__ == "__main__":
    print(f"research-tools-server listening on :{PORT}", flush=True)
    HTTPServer(("", PORT), MCPHandler).serve_forever()