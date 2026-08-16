# Reference OpenAI-compatible agent image for agent-orc:
#   ghcr.io/agentorc/agent-orc/openai-reference:<version>
#
# A minimal, framework-agnostic transport: it streams a chat request to the
# model-router (OPENAI_BASE_URL) and prints/returns the tokens. The model-router
# injects systemPrompt, tools, prior history, guardrails, built-in tool
# resolution (_done/_clarify/_spawn/_rag_search/...), spend, and checkpoints,
# so this image only ships input -> model-router -> output. Either env mode
# (AGENTORC_INPUT set -> run one turn and exit) or http mode (serve
# /healthz + POST /invoke on PORT, default 8000).
#
# Deploy an Agent with framework: openai-compatible. The operator injects
# OPENAI_BASE_URL=http://localhost:8080/v1 and OPENAI_API_KEY automatically.
# The `agentorc` SDK is installed for customization — replace /app/agent.py
# with your own SDK-based agent to bring a custom persona/system prompt.

# syntax=docker/dockerfile:1
FROM python:3.12-alpine

# Install the agent-orc Python SDK.
# In production, install from PyPI: pip install agentorc
# For local development, copy the SDK from the repo.
COPY pkg/python/agentorc/ /tmp/agentorc-sdk/
RUN pip install --no-cache-dir /tmp/agentorc-sdk/ && rm -rf /tmp/agentorc-sdk/

# Copy the agent source.
COPY examples/agent-sdk-template/agent.py /app/agent.py

USER 65534

ENTRYPOINT ["python", "/app/agent.py"]
