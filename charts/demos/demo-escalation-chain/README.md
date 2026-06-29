# Escalation Chain Demo

Multi-agent deployment pipeline with a orchestrator → sub-agent delegation
boundary and explicit role accountability at each tier.

## What This Demo Shows

An orchestrator agent receives a deployment request, plans the steps, and delegates
to specialized sub-agents (validation, staging, production).

## Deployment

```bash
skaffold run -p demo-escalation-chain
```

## Cleanup

```bash
helm uninstall demo-escalation-chain -n agent-orc-system
```
