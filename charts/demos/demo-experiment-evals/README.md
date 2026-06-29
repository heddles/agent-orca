# Demo Experiment Evals

LLM Code Generation Evaluations using declarative AgentWorkflow CRDs.

## Overview

This chart provides the same code generation benchmarks as `demo-llm-research` but uses `AgentWorkflow` CRDs instead of agent tool calls for deterministic execution. All 8 evaluation steps run in parallel:

- **Languages**: Python, Go, Fortran, JavaScript
- **Models**: Laguna XS.2 and Laguna M.1 (dual evaluation per language)

## Key Difference from demo-llm-research

| Aspect | demo-llm-research | demo-experiment-evals |
|--------|------------------|----------------------|
| Coordination | `evaluation-coordinator-agent` uses `_create_workflow` tool | Direct `AgentWorkflow` CRD definition |
| Determinism | Agent decides when to spawn children | All steps defined upfront, run immediately |
| Flexibility | Agent can adapt prompts dynamically | Fixed inputs, fixed execution order |

## Usage

```bash
# Install the chart
helm install experiment-evals charts/demos/demo-experiment-evals

# The workflow runs automatically when created
# Check results:
kubectl get agentworkflow laguna-code-eval -o yaml

# View individual step results:
kubectl get agentruns -l agentorc.agentorc.io/workflow=laguna-code-eval
```

## Workflow Structure

```
laguna-code-eval
├── python-xs2     → eval-python-xs2-agent
├── go-xs2         → eval-go-xs2-agent
├── fortran-xs2    → eval-fortran-xs2-agent
├── javascript-xs2 → eval-javascript-xs2-agent
├── python-m1      → eval-python-m1-agent
├── go-m1          → eval-go-m1-agent
├── fortran-m1     → eval-fortran-m1-agent
└── javascript-m1  → eval-javascript-m1-agent
```

All steps run in parallel with no dependencies. Results include:
- `task_id`, `language`, `model`
- `attempts` array with code, success status, error output
- `final_result` with overall success and token counts
- `correctness`: "pass" or "fail"