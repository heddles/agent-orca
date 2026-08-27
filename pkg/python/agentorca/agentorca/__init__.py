"""Python SDK for agent-orca's external APIs and agent framework.

This package provides two main classes:

- ``AgentOrca``: Client for the External Task API and ACP API (submit/list tasks, login).
- ``Agent``: SDK for writing agents that run on agent-orca via the OpenAI-compatible
  model-router endpoint. Provides built-in lifecycle tools (done, fail, ask, handoff, spawn).

Example::

    from agentorca import Agent

    agent = Agent()

    @agent.tool
    def search(query: str) -> str:
        ...

    result = agent.run(input="Find the latest news about LLMs")
"""

from __future__ import annotations

from agentorca.errors import AgentOrcaError  # noqa: E402,F401
from agentorca.agent import Agent, RunResult, ToolSpec  # noqa: E402,F401

__all__ = ["AgentOrca", "AgentOrcaError", "Agent", "RunResult", "ToolSpec"]

import io
import json
import ssl
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, Iterator, List, Optional


class AgentOrca:
	"""Client for the agent-orca External Task API and ACP API.

	endpoint: base URL of the External Task API (default http://localhost:8084).
	acp:      base URL of the ACP API      (default http://localhost:8000).
	token:    bearer token. If omitted, call ``login`` first or pass it explicitly.
	insecure: skip TLS verification (local dev only).
	"""

	def __init__(
		self,
		endpoint: str = "http://localhost:8084",
		token: Optional[str] = None,
		acp: str = "http://localhost:8000",
		timeout: float = 30.0,
		insecure: bool = False,
	):
		self.endpoint = endpoint.rstrip("/")
		self.acp = acp.rstrip("/")
		self.token = token
		self.timeout = timeout
		self._ssl = ssl._create_unverified_context() if insecure else None

	# -- low level --

	def _headers(self, extra: Optional[Dict[str, str]] = None) -> Dict[str, str]:
		h = {"User-Agent": "agentorca-py/0.1"}
		if self.token:
			h["Authorization"] = f"Bearer {self.token}"
		if extra:
			h.update(extra)
		return h

	def _request(
		self,
		method: str,
		url: str,
		body: Any = None,
		content_type: str = "application/json",
	) -> Any:
		data: Optional[bytes] = None
		if body is not None:
			data = json.dumps(body).encode()
		req = urllib.request.Request(
			url, data=data, method=method,
			headers=self._headers({"Content-Type": content_type} if content_type else {}),
		)
		try:
			with urllib.request.urlopen(req, timeout=self.timeout, context=self._ssl) as resp:  # type: ignore[arg-type]
				raw = resp.read()
				if resp.status in (204, 205):
					return None
				if not raw:
					return None
				try:
					return json.loads(raw)
				except json.JSONDecodeError:
					return raw.decode("utf-8", "replace")
		except urllib.error.HTTPError as e:
			msg = e.read().decode("utf-8", "replace").strip()
			raise AgentOrcaError(e.code, msg) from e
		except urllib.error.URLError as e:
			raise AgentOrcaError(0, str(e.reason)) from e

	def _post_form(self, url: str, fields: Dict[str, str]) -> Dict[str, Any]:
		data = urllib.parse.urlencode(fields).encode()
		req = urllib.request.Request(
			url, data=data, method="POST",
			headers=self._headers({"Content-Type": "application/x-www-form-urlencoded"}),
		)
		try:
			with urllib.request.urlopen(req, timeout=self.timeout, context=self._ssl) as resp:  # type: ignore[arg-type]
				return json.loads(resp.read())
		except urllib.error.HTTPError as e:
			msg = e.read().decode("utf-8", "replace").strip()
			raise AgentOrcaError(e.code, msg) from e
		except urllib.error.URLError as e:
			raise AgentOrcaError(0, str(e.reason)) from e

	# -- auth --

	def login(self, client_id: str, client_secret: str) -> str:
		"""Exchange client_credentials for a bearer token; caches it on the client."""
		fields = {
			"grant_type": "client_credentials",
			"client_id": client_id,
			"client_secret": client_secret,
		}
		data = self._post_form(f"{self.endpoint}/oauth/token", fields)
		tok = data.get("access_token")
		if not tok:
			raise AgentOrcaError(0, "token exchange returned no access_token")
		self.token = tok
		return tok

	# -- agents (ACP) --

	def list_agents(self) -> List[Dict[str, Any]]:
		"""List agents available to the authenticated tenant."""
		data = self._request("GET", f"{self.acp}/agents")
		return data.get("agents", []) if isinstance(data, dict) else []

	# -- tasks (External Task API) --

	def submit_task(
		self,
		agent: str,
		input: str,
		timeout: Optional[str] = None,
		callback: Optional[Dict[str, str]] = None,
		metadata: Optional[Dict[str, str]] = None,
	) -> Dict[str, Any]:
		"""Submit a task; returns the created task object."""
		sub: Dict[str, Any] = {"agent": agent, "input": input}
		if timeout:
			sub["timeout"] = timeout
		if callback:
			sub["callback"] = callback
		if metadata:
			sub["metadata"] = metadata
		resp = self._request("POST", f"{self.endpoint}/v1/tasks", body=sub)
		return resp if isinstance(resp, dict) else {}

	def get_task(self, task_id: str) -> Dict[str, Any]:
		resp = self._request("GET", f"{self.endpoint}/v1/tasks/{urllib.parse.quote(task_id)}")
		return resp if isinstance(resp, dict) else {}

	def list_tasks(self, agent: Optional[str] = None, status: Optional[str] = None) -> List[Dict[str, Any]]:
		params = {}
		if agent:
			params["agent"] = agent
		if status:
			params["status"] = status
		url = f"{self.endpoint}/v1/tasks"
		if params:
			url += "?" + urllib.parse.urlencode(params)
		data = self._request("GET", url)
		return data.get("tasks", []) if isinstance(data, dict) else []

	def cancel_task(self, task_id: str) -> None:
		self._request("DELETE", f"{self.endpoint}/v1/tasks/{urllib.parse.quote(task_id)}")

	# -- streaming --

	def stream_task(self, task_id: str) -> Iterator[Dict[str, str]]:
		"""Yield SSE events for a task as {event, data} dicts."""
		url = f"{self.endpoint}/v1/tasks/{urllib.parse.quote(task_id)}/stream"
		req = urllib.request.Request(url, method="GET", headers=self._headers({"Accept": "text/event-stream"}))
		with urllib.request.urlopen(req, timeout=self.timeout, context=self._ssl) as resp:  # type: ignore[arg-type]
			yield from self._iter_sse(resp)

	@staticmethod
	def _iter_sse(resp: Any) -> Iterator[Dict[str, str]]:
		"""Iterate an SSE stream (an object yielding bytes or str lines)."""
		event: str = ""
		data_lines: List[str] = []
		for raw in resp:
			line = raw.decode("utf-8", "replace") if isinstance(raw, (bytes, bytearray)) else raw
			line = line.rstrip("\r\n")
			if line.startswith("event:"):
				event = line[len("event:"):].strip()
			elif line.startswith("data:"):
				data_lines.append(line[len("data:"):].strip())
			elif line == "":
				if event or data_lines:
					yield {"event": event, "data": "\n".join(data_lines)}
					event = ""
					data_lines = []
			# Lines starting with ':' are SSE comments and are ignored.

	def wait_task(self, task_id: str) -> Dict[str, Any]:
		"""Stream a task to completion and return the final task object."""
		final: Dict[str, Any] = {}
		for ev in self.stream_task(task_id):
			if ev["event"] == "complete":
				try:
					final = json.loads(ev["data"])
				except json.JSONDecodeError:
					final = {"raw": ev["data"]}
				break
			if ev["event"] == "error":
				raise AgentOrcaError(0, ev["data"])
		return final
