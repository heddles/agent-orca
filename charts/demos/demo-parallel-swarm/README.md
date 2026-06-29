# Parallel Swarm Demo

Fan-out multi-agent swarm with an orchestrator spawning parallel sub-agents for
security research tasks.

## What This Demo Shows

An orchestrator dispatches multiple parallel sub-agents simultaneously (vulnerability
analysis, code review, dependency audit). Results are collected and synthesized.

## Deployment

```bash
skaffold run -p demo-parallel-swarm
```

## Cleanup

```bash
helm uninstall demo-parallel-swarm -n agent-orc-system
```
