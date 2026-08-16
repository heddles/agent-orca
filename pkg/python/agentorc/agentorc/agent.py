"""Agent SDK for agent-orc.

This module provides the ``Agent`` class — a thin wrapper around an
OpenAI-compatible client pointed at the model-router sidecar
(``http://localhost:8080`` by default).  It exposes the built-in lifecycle
tools (``_done``, ``_fail``, ``_clarify``, ``_handoff``, ``_spawn``) as
Python methods so agent code stays clean:

.. code-block:: python

    from agentorc import Agent

    agent = Agent()

    @agent.tool
    def search_web(query: str) -> str:
        ...

    result = agent.run(input="Find the latest news about LLMs")
    print(result.output)

The SDK targets the ``openai-compatible`` framework tier (Tier 1), which
injects ``OPENAI_BASE_URL=http://localhost:8080`` into the agent container.
No API key is required — the model-router handles authentication upstream.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

# AgentOrcError is defined in the package's errors module.
from agentorc.errors import AgentOrcError  # noqa: F401


# ---------------------------------------------------------------------------
# Built-in tool definitions
# ---------------------------------------------------------------------------

# These are the function definitions the model-router recognises as built-in.
# The SDK registers them so the LLM can call them; the SDK translates the
# call into the appropriate action (HTTP to the internal API, or state flag).

_DONE_TOOL = {
    "type": "function",
    "function": {
        "name": "_done",
        "description": (
            "Signal that the task is complete. Use this when you have a final answer "
            "or output to return. The run will transition to Succeeded and no further "
            "LLM calls will be made."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "output": {"type": "string", "description": "The final output/answer."},
            },
            "required": ["output"],
        },
    },
}

_FAIL_TOOL = {
    "type": "function",
    "function": {
        "name": "_fail",
        "description": (
            "Signal an unrecoverable failure with an explanation. Use this when the "
            "task cannot be completed and proceeding further would not help."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "reason": {"type": "string", "description": "Why the task failed."},
                "retryable": {"type": "boolean", "description": "Whether the failure is retryable."},
            },
            "required": ["reason"],
        },
    },
}

_CLARIFY_TOOL = {
    "type": "function",
    "function": {
        "name": "_clarify",
        "description": (
            "Ask the user for clarification on a specific point. Use this when you "
            "truly cannot proceed without additional information."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "question": {"type": "string", "description": "The question to ask the user."},
            },
            "required": ["question"],
        },
    },
}

_HANDOFF_TOOL = {
    "type": "function",
    "function": {
        "name": "_handoff",
        "description": (
            "Hand off the task to another agent. The current run ends and the "
            "handed-off agent takes over."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "agentRef": {"type": "string", "description": "Name of the agent to hand off to."},
                "input": {"type": "string", "description": "Input to pass to the handed-off agent."},
            },
            "required": ["agentRef", "input"],
        },
    },
}

_SPAWN_TOOL = {
    "type": "function",
    "function": {
        "name": "_spawn",
        "description": (
            "Spawn a child agent to work on a sub-task. The parent agent waits "
            "for the child to complete and receives its output."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "agentRef": {"type": "string", "description": "Name of the child agent."},
                "input": {"type": "string", "description": "Input for the child agent."},
                "timeoutSeconds": {"type": "integer", "description": "Timeout in seconds (default 300)."},
            },
            "required": ["agentRef", "input"],
        },
    },
}


# ---------------------------------------------------------------------------
# Data classes
# ---------------------------------------------------------------------------

@dataclass
class RunResult:
    """The final result of an agent run."""
    output: str = ""
    phase: str = ""
    spend_usd: str = ""
    failure_reason: str = ""
    completed_at: str = ""
    raw: Dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "RunResult":
        return cls(
            output=data.get("output", ""),
            phase=data.get("phase", data.get("status", "")),
            spend_usd=data.get("spendUSD", ""),
            failure_reason=data.get("failureReason", ""),
            completed_at=data.get("completedAt", ""),
            raw=data,
        )


@dataclass
class ToolSpec:
    """Definition of a custom tool the agent can call."""
    name: str
    description: str
    parameters: Dict[str, Any] = field(default_factory=dict)
    handler: Optional[Callable] = None

    def to_openai(self) -> Dict[str, Any]:
        return {
            "type": "function",
            "function": {
                "name": self.name,
                "description": self.description,
                "parameters": self.parameters,
            },
        }


# ---------------------------------------------------------------------------
# Agent
# ---------------------------------------------------------------------------

class Agent:
    """A thin agent-orc SDK that wraps an OpenAI-compatible client.

    The agent communicates with the model-router sidecar at
    ``http://localhost:8080`` (overridable via the ``OPENAI_BASE_URL``
    environment variable).  The model-router injects the built-in tools
    (``_done``, ``_fail``, ``_clarify``, ``_handoff``, ``_spawn``) into every
    conversation, and the SDK provides Pythonic helpers for calling them.

    Parameters
    ----------
    model:
        Default model identifier to pass in chat completion requests.
        If empty, the model-router selects one from the configured ModelSelector.
    base_url:
        Override the OpenAI-compatible endpoint URL.
        Defaults to ``OPENAI_BASE_URL`` env var or ``http://localhost:8080``.
    api_key:
        API key for the OpenAI-compatible endpoint.  If empty, the
        ``OPENAI_API_KEY`` env var is used.  The model-router typically
        doesn't require a key, so this can be omitted.
    system_prompt:
        System instruction prepended to every conversation.
    temperature:
        Sampling temperature (default 0.7).
    max_tokens:
        Maximum tokens to generate per turn.
    """

    def __init__(
        self,
        model: Optional[str] = None,
        base_url: Optional[str] = None,
        api_key: Optional[str] = None,
        system_prompt: str = "",
        temperature: float = 0.7,
        max_tokens: Optional[int] = None,
    ):
        self.model = model
        self.base_url = (base_url or os.environ.get("OPENAI_BASE_URL", "http://localhost:8080")).rstrip("/")
        self.api_key = api_key or os.environ.get("OPENAI_API_KEY", "")
        self.system_prompt = system_prompt
        self.temperature = temperature
        self.max_tokens = max_tokens

        self._tools: Dict[str, ToolSpec] = {}
        # Built-in tools are always available.
        self._register_builtin_tools()

        # State flags set by built-in tool handlers.
        self._done_called = False
        self._fail_called = False
        self._clarify_called = False
        self._handoff_called = False
        self._spawn_result: Optional[Dict[str, Any]] = None

    # ------------------------------------------------------------------
    # Tool registration
    # ------------------------------------------------------------------

    def tool(self, func: Optional[Callable] = None, *, name: Optional[str] = None,
             description: str = "", parameters: Optional[Dict[str, Any]] = None) -> Callable:
        """Register a function as a tool callable by the LLM.

        Can be used as a decorator::

            @agent.tool
            def search(query: str) -> str:
                \"\"\"Search the web.\"\"\"
                ...

        Or with explicit metadata::

            @agent.tool(name="search", description="Search the web.", parameters={...})
            def search(query: str) -> str:
                ...
        """
        def _register(fn: Callable) -> Callable:
            tool_name = name or fn.__name__
            tool_desc = description or (fn.__doc__ or "").strip()
            tool_params = parameters or _infer_params(fn)
            spec = ToolSpec(
                name=tool_name,
                description=tool_desc,
                parameters=tool_params,
                handler=fn,
            )
            self._tools[tool_name] = spec
            return fn

        if func is not None:
            return _register(func)
        return _register

    def _register_builtin_tools(self) -> None:
        """Register the built-in tool helpers as callable tools."""
        self._tools["_done"] = ToolSpec(
            name="_done",
            description=_DONE_TOOL["function"]["description"],
            parameters=_DONE_TOOL["function"]["parameters"],
            handler=self.done,
        )
        self._tools["_fail"] = ToolSpec(
            name="_fail",
            description=_FAIL_TOOL["function"]["description"],
            parameters=_FAIL_TOOL["function"]["parameters"],
            handler=self.fail,
        )
        self._tools["_clarify"] = ToolSpec(
            name="_clarify",
            description=_CLARIFY_TOOL["function"]["description"],
            parameters=_CLARIFY_TOOL["function"]["parameters"],
            handler=self.ask,
        )
        self._tools["_handoff"] = ToolSpec(
            name="_handoff",
            description=_HANDOFF_TOOL["function"]["description"],
            parameters=_HANDOFF_TOOL["function"]["parameters"],
            handler=self.handoff,
        )
        self._tools["_spawn"] = ToolSpec(
            name="_spawn",
            description=_SPAWN_TOOL["function"]["description"],
            parameters=_SPAWN_TOOL["function"]["parameters"],
            handler=self.spawn,
        )

    # ------------------------------------------------------------------
    # Built-in tool helpers (called by the LLM via tool execution)
    # ------------------------------------------------------------------

    def done(self, output: str = "", summary: str = "") -> Dict[str, Any]:
        """Signal that the task is complete.

        The run transitions to Succeeded. The model-router intercepts this
        tool call and notifies the operator — the agent process should
        return the tool result to the LLM and then exit.
        """
        self._done_called = True
        result = output or summary
        return {"status": "done", "output": result}

    def fail(self, reason: str, retryable: bool = False) -> Dict[str, Any]:
        """Signal an unrecoverable failure.

        The run transitions to Failed. Use this when the task cannot be
        completed and proceeding would not help.
        """
        self._fail_called = True
        return {"status": "failed", "reason": reason, "retryable": retryable}

    def ask(self, question: str) -> Dict[str, Any]:
        """Ask the user for clarification.

        The run pauses and waits for human input. The human's answer is
        delivered as the next user message.
        """
        self._clarify_called = True
        return {"status": "clarifying", "question": question}

    def handoff(self, agent_ref: str, input: str = "") -> Dict[str, Any]:
        """Hand off the task to another agent.

        The current run ends and the handed-off agent takes over with the
        given input.
        """
        self._handoff_called = True
        return {"status": "handed_off", "agent": agent_ref, "input": input}

    def spawn(self, agent_ref: str, input: str, timeout_seconds: int = 300) -> Dict[str, Any]:
        """Spawn a child agent to work on a sub-task.

        The parent agent waits for the child to complete and receives its
        output in the tool result.
        """
        self._spawn_result = {
            "agentRef": agent_ref,
            "input": input,
            "timeoutSeconds": timeout_seconds,
        }
        # The model-router intercepts _spawn and returns the child's output
        # as the tool result. We return a placeholder here.
        return {"status": "spawning", "agent": agent_ref}

    # ------------------------------------------------------------------
    # Checkpoint helpers
    # ------------------------------------------------------------------

    def load_checkpoint(self, session_id: str) -> List[Dict[str, str]]:
        """Load conversation history from the checkpoint store.

        Returns a list of ``{"role": "user"|"assistant", "content": "..."}``
        messages.  The checkpoint store is backed by Redis (or S3/GCS in
        production) and is managed by the model-router.

        This is a no-op stub when running outside agent-orc (e.g. in local
        testing).  In production, the model-router injects the checkpoint
        automatically via ``PRIOR_RUN_REF``; this method is for agents that
        want to explicitly load a different session's history.
        """
        # In production, the model-router handles checkpoint loading via
        # PriorRunRef. This method is provided for agents that want to
        # explicitly load a different session's history.
        # The checkpoint store is accessible via the internal API.
        return []

    def save_checkpoint(self, session_id: str, messages: List[Dict[str, str]]) -> str:
        """Save conversation history to the checkpoint store.

        Returns the checkpoint reference key.  In production, the
        model-router automatically checkpoints every N turns (configurable
        via ``Agent.spec.memory.checkpointEvery``); this method is for
        agents that want to explicitly save state.
        """
        # In production, the model-router handles checkpointing automatically.
        # This method is provided for explicit checkpoint management.
        return f"agentorc/runs/{session_id}/state"

    # ------------------------------------------------------------------
    # Core run loop
    # ------------------------------------------------------------------

    def run(
        self,
        input: str,
        *,
        messages: Optional[List[Dict[str, Any]]] = None,
        max_turns: int = 50,
    ) -> RunResult:
        """Run the agent on the given input until a terminal state.

        Parameters
        ----------
        input:
            The user task to execute.
        messages:
            Optional pre-loaded conversation history (from ``load_checkpoint``).
        max_turns:
            Maximum number of LLM turns before the run is forcibly stopped.

        Returns
        -------
        RunResult
            The final output, phase, and metadata.
        """
        conversation: List[Dict[str, Any]] = []
        if self.system_prompt:
            conversation.append({"role": "system", "content": self.system_prompt})
        if messages:
            conversation.extend(messages)
        conversation.append({"role": "user", "content": input})

        for turn in range(max_turns):
            response = self._chat_completion(conversation)
            if response is None:
                break

            # Check for tool calls
            tool_calls = response.get("choices", [{}])[0].get("message", {}).get("tool_calls", [])
            if not tool_calls:
                # No tool calls — add the assistant's text response and continue.
                assistant_msg = response["choices"][0]["message"]
                conversation.append(assistant_msg)
                continue

            # Execute tool calls
            for tc in tool_calls:
                tool_name = tc["function"]["name"]
                tool_args = json.loads(tc["function"]["arguments"] or "{}")
                tool = self._tools.get(tool_name)

                conversation.append({
                    "role": "assistant",
                    "tool_calls": [tc],
                })

                if tool and tool.handler:
                    try:
                        result = tool.handler(**tool_args)
                    except TypeError:
                        # Handler signature doesn't match — pass as dict.
                        result = tool.handler(tool_args)
                    tool_result = json.dumps(result) if not isinstance(result, str) else result
                else:
                    tool_result = json.dumps({"error": f"unknown tool: {tool_name}"})

                conversation.append({
                    "role": "tool",
                    "tool_call_id": tc.get("id", ""),
                    "name": tool_name,
                    "content": tool_result,
                })

            # Check terminal states
            if self._done_called:
                output = self._extract_done_output(conversation)
                return RunResult(output=output, phase="Succeeded")
            if self._fail_called:
                reason = self._extract_fail_reason(conversation)
                return RunResult(phase="Failed", failure_reason=reason)
            if self._handoff_called:
                return RunResult(phase="HandedOff")
            if self._clarify_called:
                return RunResult(phase="WaitingForInput")

        # Max turns exceeded
        return RunResult(phase="Failed", failure_reason="max turns exceeded")

    def _chat_completion(self, messages: List[Dict[str, Any]]) -> Optional[Dict[str, Any]]:
        """Send a chat completion request to the model-router."""
        body: Dict[str, Any] = {
            "messages": messages,
            "temperature": self.temperature,
        }
        if self.model:
            body["model"] = self.model
        if self.max_tokens:
            body["max_tokens"] = self.max_tokens

        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"

        data = json.dumps(body).encode()
        req = urllib.request.Request(
            f"{self.base_url}/v1/chat/completions",
            data=data,
            method="POST",
            headers=headers,
        )
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            msg = e.read().decode("utf-8", "replace").strip()
            raise AgentOrcError(e.code, msg) from e
        except urllib.error.URLError as e:
            raise AgentOrcError(0, str(e.reason)) from e

    @staticmethod
    def _extract_done_output(conversation: List[Dict[str, Any]]) -> str:
        """Extract the output from the most recent _done tool call."""
        for msg in reversed(conversation):
            if msg.get("role") == "assistant" and msg.get("tool_calls"):
                for tc in msg["tool_calls"]:
                    if tc.get("function", {}).get("name") == "_done":
                        try:
                            args = json.loads(tc["function"].get("arguments", "{}"))
                            return args.get("output", args.get("summary", ""))
                        except (json.JSONDecodeError, KeyError):
                            pass
        return ""

    @staticmethod
    def _extract_fail_reason(conversation: List[Dict[str, Any]]) -> str:
        """Extract the reason from the most recent _fail tool call."""
        for msg in reversed(conversation):
            if msg.get("role") == "assistant" and msg.get("tool_calls"):
                for tc in msg["tool_calls"]:
                    if tc.get("function", {}).get("name") == "_fail":
                        try:
                            args = json.loads(tc["function"].get("arguments", "{}"))
                            return args.get("reason", "")
                        except (json.JSONDecodeError, KeyError):
                            pass
        return ""


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _infer_params(func: Callable) -> Dict[str, Any]:
    """Infer a JSON schema for a function's parameters from its type hints."""
    import inspect
    sig = inspect.signature(func)
    properties: Dict[str, Any] = {}
    required: List[str] = []
    type_map = {
        str: {"type": "string"},
        int: {"type": "integer"},
        float: {"type": "number"},
        bool: {"type": "boolean"},
        list: {"type": "array"},
        dict: {"type": "object"},
    }
    for name, param in sig.parameters.items():
        if param.annotation in type_map:
            properties[name] = type_map[param.annotation]
        else:
            properties[name] = {"type": "string"}
        if param.default is inspect.Parameter.empty:
            required.append(name)
    return {
        "type": "object",
        "properties": properties,
        "required": required,
    }
