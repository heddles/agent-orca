# Financial Analysis Demo

AI financial analyst with access to market data, financial planning knowledge
bases, and MCP-sourced financial tools.

## What This Demo Shows

A single financial analyst agent that can answer investment questions, analyze portfolios,
and pull live market data via MCP tools.

## Deployment

```bash
skaffold run -p demo-financial-analysis
```

## Cleanup

```bash
helm uninstall demo-financial-analysis -n agent-orca-system
```
