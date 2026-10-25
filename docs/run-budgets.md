# Run budgets

An AgentRun can have an enforced spend cap. `spec.budget` makes the run **warn** as it nears the
cap and then **fail** or **pause for approval** when it reaches it. The ModelSelector's
`budgetCap.perRun` is different: it only steers routing toward cheaper models and never stops a run.

```yaml
spec:
  budget:
    maxUSD: "2.00"           # the cap
    warnAtPercent: 80        # default 80
    onExceeded: pause        # fail (default) | pause
    raiseByUSD: "1.50"       # what one approval adds; defaults to maxUSD
    pauseTimeout: 1h         # how long a paused run waits; default 24h
    approverGroups: [finance] # empty: anyone with access to the namespace
```

A full example is `config/samples/agentrun-budget.yaml`.

## What happens

1. **Warning.** When spend passes `warnAtPercent` of the cap, the run gets a `budget` event in its
   trace, a Kubernetes `BudgetWarning` event and the `BudgetWarning` condition. This happens once
   per cap.
2. **At the cap, `onExceeded: fail`.** The model-router halts the run before the next model call,
   exactly like a [safeguard](crds.md) trip: the run fails with `safeguard: budget of $2.00
   reached ($2.03 spent)` and is not retried.
3. **At the cap, `onExceeded: pause`.** The router holds the agent's request instead. The agent
   process, its sandbox and its workspace stay alive; nothing is lost. The operator opens an
   approval of kind `budget` in the Approvals inbox (and sends the usual webhook notification).
   - **Approve:** the cap rises by `raiseByUSD`, the router sees it within about 10 seconds, and
     the run carries on. If it reaches the new cap, it asks again, as a new request.
   - **Reject, or no decision within `pauseTimeout`:** the run fails with the reason and who
     decided.

Approving needs a signed-in user in one of `approverGroups`, like any other approval, and the
decision is recorded in PostgreSQL with who made it.

## Where to see it

- The run's trace shows `budget` events (warning, reached, raised, declined); the **Budget**
  filter isolates them.
- `kubectl get agentrun <name> -o jsonpath='{.status.budget}'` shows the state (`ok`, `warning`,
  `exceeded`), the current cap, the last reported spend and the number of raises.
- Conditions `BudgetWarning` and `BudgetExceeded` (True while a run waits for a decision).

## Notes

- **The router owns the count, the operator owns the cap.** A run cannot raise its own budget: the
  router reports state and spend, and only an approved raise changes `status.budget.capUSD`. A
  restarted router reads the current cap back, so raises survive pod restarts.
- **Spend is what the router can price.** It counts the cost of model calls from the providers'
  `costPerInputToken` and `costPerOutputToken`. A provider without prices counts as free, and a
  call that is already running when the cap is reached completes, so a run can overshoot the cap
  by up to one call.
- **Pausing needs PostgreSQL.** Approvals are stored there. Without it, `onExceeded: pause` behaves
  like `fail`.
- **The run's `timeout` keeps counting during a pause.** Set it to cover the longest pause you
  accept.
