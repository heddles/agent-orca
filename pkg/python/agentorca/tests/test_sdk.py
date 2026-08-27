import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import agentorca


class _StubHandler(BaseHTTPRequestHandler):
	# Subclasses (or the factory below) set `routes`.
	routes = {}

	def log_message(self, *a, **kw):
		pass

	def _send(self, code, body=b"", ctype="application/json"):
		self.send_response(code)
		self.send_header("Content-Type", ctype)
		self.send_header("Content-Length", str(len(body)))
		self.end_headers()
		if body:
			self.wfile.write(body)

	def do_GET(self):  # noqa: N802
		self._route("GET")

	def do_POST(self):  # noqa: N802
		length = int(self.headers.get("Content-Length", "0") or "0")
		self._body = self.rfile.read(length) if length else b""
		self._route("POST")

	def do_DELETE(self):  # noqa: N802
		length = int(self.headers.get("Content-Length", "0") or "0")
		if length:
			self.rfile.read(length)
		self._route("DELETE")

	def _route(self, method):
		# Match on the path without its query string so list filters match.
		path = self.path.split("?", 1)[0]
		key = (method, path)
		handler = self.routes.get(key)
		if not handler:
			self._send(404, b'{"error":"not found"}')
			return
		handler(self)


def _start_server(routes):
	# Build a handler class with the given routes bound.
	handler = type("H", (_StubHandler,), {"routes": routes})
	srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
	threading.Thread(target=srv.serve_forever, daemon=True).start()
	return srv


class SSEStub:
	"""A route handler that emits a fixed SSE payload then closes."""

	def __init__(self, chunks):
		self.chunks = chunks

	def __call__(self, h):
		h.send_response(200)
		h.send_header("Content-Type", "text/event-stream")
		h.end_headers()
		for chunk in self.chunks:
			h.wfile.write(chunk)
			if hasattr(h.wfile, "flush"):
				h.wfile.flush()


class TestSDK(unittest.TestCase):
	def test_iter_sse_parses_events(self):
		# _iter_sse consumes a line iterator (as http.client.HTTPResponse yields),
		# one SSE line per element, including the trailing blank separator line.
		raw = [
			b"event: status\n",
			b'data: {"phase":"Running"}\n',
			b"\n",
			b"event: token\n",
			b"data: hello\n",
			b"\n",
			b"event: complete\n",
			b'data: {"id":"t1"}\n',
			b"\n",
		]
		events = list(agentorca.AgentOrca._iter_sse(raw))
		self.assertEqual(len(events), 3)
		self.assertEqual(events[0]["event"], "status")
		self.assertEqual(events[1]["data"], "hello")
		self.assertEqual(events[2]["data"], '{"id":"t1"}')

	def test_iter_sse_strips_crlf(self):
		raw = [b"event: token\r\n", b"data: x\r\n", b"\r\n"]
		events = list(agentorca.AgentOrca._iter_sse(raw))
		self.assertEqual(events, [{"event": "token", "data": "x"}])

	def test_iter_sse_ignores_comment_lines(self):
		raw = [b": keep-alive\n", b"event: token\n", b"data: hi\n", b"\n"]
		events = list(agentorca.AgentOrca._iter_sse(raw))
		self.assertEqual(events, [{"event": "token", "data": "hi"}])

	def test_login(self):
		def handler(h):
			body = json.dumps({"access_token": "tk123", "token_type": "Bearer", "expires_in": 3600}).encode()
			h._send(200, body)
		srv = _start_server({("POST", "/oauth/token"): handler})
		try:
			c = agentorca.AgentOrca(endpoint=srv.server_address[0] and f"http://127.0.0.1:{srv.server_address[1]}")
			tok = c.login("cid", "secret")
			self.assertEqual(tok, "tk123")
			self.assertEqual(c.token, "tk123")
		finally:
			srv.shutdown()

	def test_submit_task_and_get(self):
		def submit(h):
			h._send(201, json.dumps({"id": "t1", "agent": "a", "status": "Pending",
				"links": {"self": "/v1/tasks/t1", "stream": "/v1/tasks/t1/stream"}}).encode())
		def gettask(h):
			h._send(200, json.dumps({"id": "t1", "status": "Succeeded", "output": "hi"}).encode())
		srv = _start_server({("POST", "/v1/tasks"): submit, ("GET", "/v1/tasks/t1"): gettask})
		url = f"http://127.0.0.1:{srv.server_address[1]}"
		try:
			c = agentorca.AgentOrca(endpoint=url, token="tk")
			created = c.submit_task(agent="a", input="hi")
			self.assertEqual(created["id"], "t1")
			self.assertEqual(created["links"]["stream"], "/v1/tasks/t1/stream")
			got = c.get_task("t1")
			self.assertEqual(got["status"], "Succeeded")
		finally:
			srv.shutdown()

	def test_list_tasks_and_cancel(self):
		def lst(h):
			h._send(200, json.dumps({"tasks": [{"id": "t1", "agent": "a", "status": "Pending"}], "count": 1}).encode())
		def cancel(h):
			h._send(204)
		srv = _start_server({("GET", "/v1/tasks"): lst, ("DELETE", "/v1/tasks/t1"): cancel})
		url = f"http://127.0.0.1:{srv.server_address[1]}"
		try:
			c = agentorca.AgentOrca(endpoint=url, token="tk")
			tasks = c.list_tasks(agent="a", status="Pending")
			self.assertEqual(len(tasks), 1)
			self.assertEqual(tasks[0]["id"], "t1")
			self.assertIsNone(c.cancel_task("t1"))
		finally:
			srv.shutdown()

	def test_list_agents(self):
		def ag(h):
			h._send(200, json.dumps({"agents": [{"name": "bob", "description": "d"}]}).encode())
		srv = _start_server({("GET", "/agents"): ag})
		url = f"http://127.0.0.1:{srv.server_address[1]}"
		try:
			c = agentorca.AgentOrca(acp=url, token="tk")
			agents = c.list_agents()
			self.assertEqual(len(agents), 1)
			self.assertEqual(agents[0]["name"], "bob")
		finally:
			srv.shutdown()

	def test_submit_error_raises(self):
		def handler(h):
			h._send(429, b'{"error":"rate limit exceeded"}')
			h.headers["Retry-After"] = "59"
		srv = _start_server({("POST", "/v1/tasks"): handler})
		url = f"http://127.0.0.1:{srv.server_address[1]}"
		try:
			c = agentorca.AgentOrca(endpoint=url, token="tk")
			with self.assertRaises(agentorca.AgentOrcaError) as cm:
				c.submit_task(agent="a", input="hi")
			self.assertEqual(cm.exception.status, 429)
		finally:
			srv.shutdown()

	def test_wait_task_completes(self):
		chunks = [b"event: token\ndata: hello\n\n", b"event: complete\ndata: {\"id\":\"t1\",\"status\":\"Succeeded\"}\n\n"]
		srv = _start_server({("GET", "/v1/tasks/t1/stream"): SSEStub(chunks)})
		url = f"http://127.0.0.1:{srv.server_address[1]}"
		try:
			c = agentorca.AgentOrca(endpoint=url, token="tk")
			final = c.wait_task("t1")
			self.assertEqual(final["status"], "Succeeded")
		finally:
			srv.shutdown()

	def test_wait_task_error_event(self):
		chunks = [b"event: error\ndata: something went wrong\n\n"]
		srv = _start_server({("GET", "/v1/tasks/t1/stream"): SSEStub(chunks)})
		url = f"http://127.0.0.1:{srv.server_address[1]}"
		try:
			c = agentorca.AgentOrca(endpoint=url, token="tk")
			with self.assertRaises(agentorca.AgentOrcaError):
				c.wait_task("t1")
		finally:
			srv.shutdown()


if __name__ == "__main__":
	unittest.main()
