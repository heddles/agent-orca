# Causal Discovery Microscope Demo

An interactive AI system for real-time causal hypothesis generation and testing using a Mixture of Experts architecture with live research integration and dynamic evidence visualization.

## Architecture

```
Browser (Causal Discovery UI)
   │
   ▼
CausalDiscoveryUI Pod (Caddy + React SPA)
├── serves React SPA on :8080
└── /api/* → proxies to agent-orc resources

agent-orc Operator (port 8083)
   │
   ▼
Expert Agents + MCP Servers + KnowledgeBases
```

## Components

### UI (charts/demos/demo-causal-discovery/ui/)
- React SPA with observation input, expert panels, and causal graph visualization
- Built with Vite, served by Caddy

### Proxy (charts/demos/demo-causal-discovery/)
- Caddy-based reverse proxy serving the UI
- Multi-stage Dockerfile builds the React SPA and serves with Caddy

### Expert Agents
- **causal-orchestrator**: Coordinates multiple domain experts
- **epidemiology-expert**: Epidemiology causal inference specialist
- **economics-expert**: Economics causal relationships specialist  
- **neuroscience-expert**: Neuroscience causal networks specialist

### MCP Servers
- **causal-mcp**: Hypothesis generation, scoring, and confidence updating
- **external-research-mcp** (Phase 2): Academic database search

### Knowledge Bases
- **causal-inference-kb**: Foundational causal inference concepts
- **epidemiology-kb**: Disease causation, study design
- **economics-kb**: Granger causality, instrumental variables
- **neuroscience-kb**: Brain connectivity, interventions

## Prerequisites

- agent-orc operator running
- `default` and `embeddings` ModelSelectors deployed
- Ollama embeddings configured

## Deploy

```bash
# Start the platform (operator + model-providers)
skaffold dev -p dev

# Deploy the causal discovery demo
skaffold run -p demo-causal-discovery

# Port-forward the UI
kubectl port-forward svc/causal-discovery-ui -n agent-orc-system 8085:80
```

Then open http://localhost:8085 in your browser.

## Usage

1. Enter an observational pattern (e.g., "Higher vitamin D levels correlate with reduced respiratory infections")
2. Click "Generate Hypotheses" 
3. The causal-orchestrator agent will:
   - Call `propose_hypothesis` to generate causal graph
   - Search knowledge bases for context
   - Score evidence using domain experts
   - Return confidence levels and explanations
4. Adjust expert weights using sliders to explore robustness
5. View the interactive causal graph and evidence dashboard

## Development

The UI can be developed locally:

```bash
cd charts/demos/demo-causal-discovery/ui
npm install
npm run dev
```

This starts Vite dev server on port 8081 with API proxy to localhost:8080.