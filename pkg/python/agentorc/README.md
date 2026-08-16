agentorc — Python SDK for the agent-orc external APIs.

A small, dependency-free (stdlib only) client for submitting tasks to
agent-orc's External Task API (port 8084) and discovering agents via the ACP
API (port 8000).

Install (development):

```bash
pip install ./pkg/python/agentorc
```

Usage:

```python
from agentorc import AgentOrc

ao = AgentOrc(endpoint="https://agent-orc.example.com:8084", acp="https://agent-orc.example.com:8000")
ao.login(client_id="acme-client", client_secret="...")
agents = ao.list_agents()
run = ao.submit_task(agent="support-bot", input="how do I reset my password?")
print(run["id"], run["status"])
ao.stream_task(run["id"])            # generator yielding SSE events
final = ao.wait_task(run["id"])      # blocks until terminal, returns final task
```
"""
