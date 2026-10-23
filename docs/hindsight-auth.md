# Hindsight API authentication

Hindsight's REST (`/v1`) and MCP (`/mcp`) endpoints expose destructive tools
(`delete_bank`, `clear_memories`, `retain`, `update_memory`, ...). Agents recall
from these banks and act on the results, so an unauthenticated endpoint lets
anyone read, wipe or poison agent memory. Authentication must be enforced by
hindsight itself, not only by an ingress in front of it.

## Bundled Hindsight (default)

The chart deploys Hindsight by default (`hindsight.enabled: true`) and wires auth for
you. The `hindsight:` values block is shared: upstream chart values (`api.*`,
`worker.*`, `postgresql.*`, ...) and the operator's client settings (`url`,
`apiKeySecret`, `bankIdTemplate`, ...) live side by side.

- The chart creates Secret `hindsight-auth` (`hindsight.existingSecret`) with a
  generated `HINDSIGHT_API_TENANT_API_KEY`, the same value as
  `HINDSIGHT_CP_DATAPLANE_API_KEY`, and a `HINDSIGHT_CP_ACCESS_KEY`. Existing entries
  are preserved on upgrade and the Secret is kept on uninstall
  (`helm.sh/resource-policy: keep`), so a rotated key or one you add survives.
- The API runs with `ApiKeyTenantExtension`, so `/v1` and `/mcp` both reject requests
  without the key. The operator reads the same key (`hindsight.apiKeySecret` defaults
  to `hindsight-auth`/`HINDSIGHT_API_TENANT_API_KEY`) and `hindsight.url` defaults to
  the bundled service, so nothing else needs configuring.
- Hindsight needs an LLM. Set the provider in `hindsight.api.env`
  (`HINDSIGHT_API_LLM_BASE_URL`, `HINDSIGHT_API_LLM_MODEL`) and add its key to the
  Secret out of band, then restart the API:
  `kubectl -n <ns> patch secret hindsight-auth -p '{"stringData":{"HINDSIGHT_API_LLM_API_KEY":"..."}}'`.
- Read the generated key (for an MCP client) with
  `kubectl -n <ns> get secret hindsight-auth -o jsonpath='{.data.HINDSIGHT_API_TENANT_API_KEY}' | base64 -d`.
- No resource requests or limits are set by default. Size them for your data: the
  API and worker can exceed 1Gi on a bank with a few hundred facts and will be
  OOM-killed under a limit that small. Set `hindsight.api.resources` and
  `hindsight.worker.resources`.
- Bring your own Secret with `hindsight.auth.manageSecret=false` and
  `hindsight.existingSecret=<name>`; it must contain `HINDSIGHT_API_TENANT_API_KEY` and
  `HINDSIGHT_CP_DATAPLANE_API_KEY` (same value).
- External Hindsight: `hindsight.enabled=false`, `hindsight.url=...` and, if it needs a
  key, `hindsight.apiKeySecret.{name,key}`.

The sections below describe the external case.

## How agent-orca authenticates

When `hindsight.apiKeySecret.name` is set in the chart:

1. The operator reads the key from that Secret (`HINDSIGHT_API_KEY` in its own env).
2. Before it creates router pods, it mirrors the key into the run namespace as the
   Secret `agentorca-hindsight-api-key` (key `HINDSIGHT_API_KEY`). Router pods run
   in tenant namespaces and cannot reference the operator's Secret directly.
3. Only the `model-router` container mounts it (as `HINDSIGHT_API_KEY`). The agent
   container and the per-run token Secret it is `envFrom`-ed with never carry it.
4. The router sends `Authorization: Bearer <key>` on every hindsight call (bank
   create, recall, retain). The key is read from the environment only; it is never
   written to the router ConfigMap.

When the value is empty nothing is sent, so unauthenticated hindsight deployments
keep working.

## Rollout

```bash
# 1. Create the key in the operator namespace (do not put it in Helm values).
kubectl -n agent-orca-system create secret generic hindsight-api-key \
  --from-literal=api-key="$(openssl rand -hex 32)"

# 2. Point agent-orca at it.
helm upgrade agent-orca charts/agent-orca --reuse-values \
  --set hindsight.apiKeySecret.name=hindsight-api-key

# 3. Enable tenant auth in the hindsight release, using the SAME key:
#    HINDSIGHT_API_TENANT_EXTENSION=hindsight_api.extensions.builtin.tenant:ApiKeyTenantExtension
#    HINDSIGHT_API_TENANT_API_KEY=<key>
#    and, so the hindsight UI keeps working,
#    controlPlane.secrets.HINDSIGHT_CP_DATAPLANE_API_KEY=<key>
```

Roll agent-orca first (step 2), then enable auth on hindsight (step 3): the router
keeps working in both orders because it sends the header either way.

Once hindsight enforces keys, remove the ingress basic-auth middleware from `/v1`
and `/mcp` (it competes for the `Authorization` header) and manage the
`IngressRoute` in-repo so a route cannot drift unauthenticated again.

## Verify

```bash
H=https://<hindsight-host>
# Must all return 401:
curl -s -o /dev/null -w '%{http_code}\n' $H/v1/default/banks
curl -s -o /dev/null -w '%{http_code}\n' -X POST $H/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
# Must succeed:
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $KEY" $H/v1/default/banks
```

Hindsight's docs imply but do not state that tenant auth covers the MCP transport,
so always test `/mcp` explicitly. An agent run that recalls/retains should still
emit `hindsight` trace events without `failed: true`.

## Rotation

1. Update the `hindsight-api-key` Secret and the hindsight release.
2. Restart the operator (it reads the key at start); it rewrites the mirrored
   Secret in each namespace on the next reconcile.
3. Recycle warm pods: containers read `HINDSIGHT_API_KEY` at start, so running pods
   keep the old key until replaced. Per-run pods pick up the new key automatically.
