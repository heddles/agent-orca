# SOC Triage Demo

AI-assisted Security Operations Center triage with human-in-the-loop escalation
and tool access.

## What This Demo Shows

A multi-agent SOC pipeline where an orchestrator triages security alerts and
delegates to specialized sub-agents (threat intel, log analysis, incident response).

## Deployment

```bash
skaffold run -p demo-soc-triage
```

## Cleanup

```bash
helm uninstall demo-soc-triage -n agent-orc-system
```
