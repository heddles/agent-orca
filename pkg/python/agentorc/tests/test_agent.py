"""Tests for the agentorc.Agent SDK class."""

import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from agentorc import Agent, RunResult


class _StubHandler(BaseHTTPRequestHandler):
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

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0") or "0")
        self._body = self.rfile.read(length) if length else b""
        path = self.path.split("?", 1)[0]
        key = ("POST", path)
        handler = self.routes.get(key)
        if not handler:
            self._send(404, b'{"error":"not found"}')
            return
        handler(self)


def _start_server(routes):
    handler = type("H", (_StubHandler,), {"routes": routes})
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


class TestAgentSDK(unittest.TestCase):
    def test_agent_registers_builtin_tools(self):
        a = Agent()
        for name in ["_done", "_fail", "_clarify", "_handoff", "_spawn"]:
            self.assertIn(name, a._tools, f"builtin tool {name} not registered")

    def test_done_helper(self):
        a = Agent()
        result = a.done(output="Task complete!")
        self.assertTrue(a._done_called)
        self.assertEqual(result["status"], "done")
        self.assertEqual(result["output"], "Task complete!")

    def test_fail_helper(self):
        a = Agent()
        result = a.fail(reason="something broke", retryable=True)
        self.assertTrue(a._fail_called)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["reason"], "something broke")
        self.assertTrue(result["retryable"])

    def test_ask_helper(self):
        a = Agent()
        result = a.ask(question="What is your name?")
        self.assertTrue(a._clarify_called)
        self.assertEqual(result["status"], "clarifying")
        self.assertEqual(result["question"], "What is your name?")

    def test_handoff_helper(self):
        a = Agent()
        result = a.handoff(agent_ref="research-agent", input="research this")
        self.assertTrue(a._handoff_called)
        self.assertEqual(result["status"], "handed_off")
        self.assertEqual(result["agent"], "research-agent")

    def test_spawn_helper(self):
        a = Agent()
        result = a.spawn(agent_ref="child-agent", input="do subtask", timeout_seconds=120)
        self.assertEqual(result["status"], "spawning")
        self.assertEqual(result["agent"], "child-agent")

    def test_tool_decorator_registers_tool(self):
        a = Agent()

        @a.tool
        def search(query: str) -> str:
            """Search the web."""
            return "results"

        self.assertIn("search", a._tools)
        self.assertEqual(a._tools["search"].description, "Search the web.")
        self.assertIn("query", a._tools["search"].parameters["properties"])

    def test_tool_decorator_with_explicit_metadata(self):
        a = Agent()

        @a.tool(name="web_search", description="Search the web", parameters={
            "type": "object",
            "properties": {"query": {"type": "string"}},
            "required": ["query"],
        })
        def search(query: str) -> str:
            return "results"

        self.assertIn("web_search", a._tools)
        self.assertEqual(a._tools["web_search"].name, "web_search")

    def test_run_with_done_tool(self):
        """Test that run() correctly processes a _done tool call and returns Succeeded."""
        a = Agent(base_url="http://127.0.0.1:1")  # invalid URL, won't be used

        def fake_chat(messages):
            # First turn: LLM calls _done
            if len(messages) == 1:  # just the user message
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "content": "",
                            "tool_calls": [{
                                "id": "call_1",
                                "type": "function",
                                "function": {
                                    "name": "_done",
                                    "arguments": json.dumps({"output": "Final answer"}),
                                },
                            }],
                        }
                    }]
                }
            return None

        a._chat_completion = fake_chat
        result = a.run(input="test")
        self.assertEqual(result.phase, "Succeeded")
        self.assertEqual(result.output, "Final answer")

    def test_run_with_fail_tool(self):
        """Test that run() correctly processes a _fail tool call and returns Failed."""
        a = Agent(base_url="http://127.0.0.1:1")

        def fake_chat(messages):
            if len(messages) == 1:
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "tool_calls": [{
                                "id": "call_1",
                                "type": "function",
                                "function": {
                                    "name": "_fail",
                                    "arguments": json.dumps({"reason": "bad input", "retryable": False}),
                                },
                            }],
                        }
                    }]
                }
            return None

        a._chat_completion = fake_chat
        result = a.run(input="test")
        self.assertEqual(result.phase, "Failed")
        self.assertEqual(result.failure_reason, "bad input")

    def test_run_with_clarify_tool(self):
        """Test that run() correctly processes a _clarify tool call."""
        a = Agent(base_url="http://127.0.0.1:1")

        def fake_chat(messages):
            if len(messages) == 1:
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "tool_calls": [{
                                "id": "call_1",
                                "type": "function",
                                "function": {
                                    "name": "_clarify",
                                    "arguments": json.dumps({"question": "What do you mean?"}),
                                },
                            }],
                        }
                    }]
                }
            return None

        a._chat_completion = fake_chat
        result = a.run(input="test")
        self.assertEqual(result.phase, "WaitingForInput")

    def test_run_with_handoff_tool(self):
        """Test that run() correctly processes a _handoff tool call."""
        a = Agent(base_url="http://127.0.0.1:1")

        def fake_chat(messages):
            if len(messages) == 1:
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "tool_calls": [{
                                "id": "call_1",
                                "type": "function",
                                "function": {
                                    "name": "_handoff",
                                    "arguments": json.dumps({"agentRef": "other-agent", "input": "take over"}),
                                },
                            }],
                        }
                    }]
                }
            return None

        a._chat_completion = fake_chat
        result = a.run(input="test")
        self.assertEqual(result.phase, "HandedOff")

    def test_run_with_custom_tool(self):
        """Test that run() executes custom tools and continues."""
        a = Agent(base_url="http://127.0.0.1:1")

        @a.tool
        def echo(text: str) -> str:
            """Echo the text back."""
            return text

        call_count = [0]

        def fake_chat(messages):
            call_count[0] += 1
            if call_count[0] == 1:
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "tool_calls": [{
                                "id": "call_1",
                                "type": "function",
                                "function": {
                                    "name": "echo",
                                    "arguments": json.dumps({"text": "hello"}),
                                },
                            }],
                        }
                    }]
                }
            elif call_count[0] == 2:
                # LLM sees the tool result and calls _done
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "tool_calls": [{
                                "id": "call_2",
                                "type": "function",
                                "function": {
                                    "name": "_done",
                                    "arguments": json.dumps({"output": "echoed: hello"}),
                                },
                            }],
                        }
                    }]
                }
            return None

        a._chat_completion = fake_chat
        result = a.run(input="echo hello")
        self.assertEqual(result.phase, "Succeeded")
        self.assertEqual(result.output, "echoed: hello")

    def test_run_max_turns_exceeded(self):
        """Test that run() returns Failed when max_turns is exceeded."""
        a = Agent(base_url="http://127.0.0.1:1")

        def fake_chat(messages):
            return {
                "choices": [{
                    "message": {
                        "role": "assistant",
                        "content": "thinking...",
                    }
                }]
            }

        a._chat_completion = fake_chat
        result = a.run(input="test", max_turns=3)
        self.assertEqual(result.phase, "Failed")
        self.assertIn("max turns", result.failure_reason)

    def test_run_no_tool_calls_continues(self):
        """Test that run() continues when the LLM returns text without tool calls."""
        a = Agent(base_url="http://127.0.0.1:1")

        call_count = [0]

        def fake_chat(messages):
            call_count[0] += 1
            if call_count[0] == 1:
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "content": "Let me think about this...",
                        }
                    }]
                }
            elif call_count[0] == 2:
                return {
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "tool_calls": [{
                                "id": "call_1",
                                "type": "function",
                                "function": {
                                    "name": "_done",
                                    "arguments": json.dumps({"output": "done"}),
                                },
                            }],
                        }
                    }]
                }
            return None

        a._chat_completion = fake_chat
        result = a.run(input="test")
        self.assertEqual(result.phase, "Succeeded")
        self.assertEqual(result.output, "done")

    def test_run_result_from_dict(self):
        data = {
            "output": "test output",
            "phase": "Succeeded",
            "spendUSD": "0.05",
            "failureReason": "",
            "completedAt": "2024-01-01T00:00:00Z",
        }
        r = RunResult.from_dict(data)
        self.assertEqual(r.output, "test output")
        self.assertEqual(r.phase, "Succeeded")
        self.assertEqual(r.spend_usd, "0.05")
        self.assertEqual(r.completed_at, "2024-01-01T00:00:00Z")

    def test_checkpoint_helpers(self):
        a = Agent()
        # These are stubs but should not raise.
        msgs = a.load_checkpoint("test-session")
        self.assertEqual(msgs, [])
        ref = a.save_checkpoint("test-session", [{"role": "user", "content": "hi"}])
        self.assertIn("test-session", ref)

    def test_env_var_base_url(self):
        import os
        os.environ["OPENAI_BASE_URL"] = "http://test-router:9090"
        try:
            a = Agent()
            self.assertEqual(a.base_url, "http://test-router:9090")
        finally:
            del os.environ["OPENAI_BASE_URL"]


if __name__ == "__main__":
    unittest.main()
