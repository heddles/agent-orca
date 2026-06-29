# Codebase Expert Demo

AI code assistant with access to source-code knowledge bases and GitHub
MCP tools.

## What This Demo Shows

A single codebase expert agent that answers questions about a proprietary codebase
using RAG over an indexed code knowledge base and GitHub MCP tools.

## Deployment

```bash
skaffold run -p demo-codebase-expert
```

## Cleanup

```bash
helm uninstall demo-codebase-expert -n agent-orc-system
```
