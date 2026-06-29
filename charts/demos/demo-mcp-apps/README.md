# MCP Apps Demo

AI SRE agent using MCP tools for infrastructure operations.

## What This Demo Shows

A single SRE agent that invokes MCP tools (kubectl, GitHub, Slack, PagerDuty) to
investigate and respond to incidents. 

## Deployment

```bash
skaffold run -p demo-mcp-apps
```

## Cleanup

```bash
helm uninstall demo-mcp-apps -n agent-orc-system
```
