"""Shared error types for the agent-orca Python SDK."""

from __future__ import annotations


class AgentOrcaError(Exception):
    """Raised on a non-success HTTP response or transport error."""

    def __init__(self, status: int, message: str):
        super().__init__(f"HTTP {status}: {message}")
        self.status = status
        self.message = message
