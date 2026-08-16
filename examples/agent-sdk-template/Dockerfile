# Reference agent using the agent-orc Python SDK.
#
# This Dockerfile builds an agent image that uses the `agentorc` Python SDK
# to communicate with the model-router sidecar (http://localhost:8080).
#
# Deploy as an Agent with framework: openai-compatible.
# The model-router injects OPENAI_BASE_URL=http://localhost:8080 automatically.

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
