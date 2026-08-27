# Demo MCP Server

A minimal MCP server for testing the `MCPServer` CRD end-to-end. It implements
the [Model Context Protocol](https://modelcontextprotocol.io/) JSON-RPC 2.0
over HTTP using only the Python 3 standard library (no dependencies).

Every tool returns live data that an LLM cannot fabricate, proving the tool
was actually invoked.

## Tools Provided

| Tool            | Description                                | Why the LLM can't fake it              |
|-----------------|--------------------------------------------|----------------------------------------|
| `current_time`  | Return the current UTC time                | LLM has no access to a real clock      |
| `random_number` | Generate a random integer in [min, max]    | LLM can't produce true randomness      |
| `dns_lookup`    | Resolve a hostname via cluster DNS         | LLM can't query live DNS               |
| `reverse`       | Reverse a string                           | LLMs struggle with character-level ops |

## Testing Locally (No Kubernetes)

Run the server:

```bash
python3 testdata/mcp-server/server.py
```

In another terminal, send JSON-RPC requests directly:

```bash
# Initialize the MCP session
curl -s http://localhost:3000 -H 'Content-Type: application/json' -d '{
  "jsonrpc": "2.0", "id": 1, "method": "initialize",
  "params": {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "test"}}
}'

# List available tools
curl -s http://localhost:3000 -H 'Content-Type: application/json' -d '{
  "jsonrpc": "2.0", "id": 2, "method": "tools/list"
}'

# Get the current time
curl -s http://localhost:3000 -H 'Content-Type: application/json' -d '{
  "jsonrpc": "2.0", "id": 3, "method": "tools/call",
  "params": {"name": "current_time", "arguments": {}}
}'
# => {"jsonrpc": "2.0", "id": 3, "result": {"content": [{"type": "text", "text": "2026-03-21T..."}], "isError": false}}

# Generate a random number
curl -s http://localhost:3000 -H 'Content-Type: application/json' -d '{
  "jsonrpc": "2.0", "id": 4, "method": "tools/call",
  "params": {"name": "random_number", "arguments": {"min": 1, "max": 1000}}
}'

# Reverse a string
curl -s http://localhost:3000 -H 'Content-Type: application/json' -d '{
  "jsonrpc": "2.0", "id": 5, "method": "tools/call",
  "params": {"name": "reverse", "arguments": {"text": "hello world"}}
}'
# => {"jsonrpc": "2.0", "id": 5, "result": {"content": [{"type": "text", "text": "dlrow olleh"}], "isError": false}}
```

## Testing In-Cluster (kind)

### Prerequisites

- A running kind cluster with the agent-orca operator deployed
- CRDs installed (`make install`)
- `shared.yaml` applied (`kubectl apply -f testdata/agents/shared.yaml`)

### Steps

1. **Build and load the image:**

   ```bash
   docker build -t demo-mcp-server:latest testdata/mcp-server/
   kind load docker-image demo-mcp-server:latest
   ```

2. **Deploy everything:**

   ```bash
   kubectl apply -f testdata/agents/mcpserver.yaml
   ```

   This creates:
   - `Deployment/demo-mcp-server` — the MCP server pod
   - `Service/demo-mcp-server` — cluster-internal endpoint on port 3000
   - `MCPServer/demo-mcp` — declares 4 tools; the controller creates child Tool CRs
   - `Agent/test-mcpserver-agent` — references the generated tools
   - `AgentRun/test-mcpserver-run` — asks for the current UTC time

3. **Verify the MCP server is running:**

   ```bash
   kubectl get pods -l app=demo-mcp-server
   # NAME                               READY   STATUS    RESTARTS   AGE
   # demo-mcp-server-...                1/1     Running   0          10s
   ```

4. **Verify the MCPServer CRD and generated Tools:**

   ```bash
   kubectl get mcpservers
   # NAME       TRANSPORT   READY   TOOLS   AGE
   # demo-mcp   http        true    4       10s

   kubectl get tools -l agentorca.io/managed-by=mcpserver
   # NAME                       AGE
   # demo-mcp-current-time      10s
   # demo-mcp-random-number     10s
   # demo-mcp-dns-lookup        10s
   # demo-mcp-reverse           10s
   ```

5. **Check the AgentRun output:**

   ```bash
   kubectl get agentrun test-mcpserver-run -o jsonpath='{.status.output}'
   ```

   The run asks "What is the current UTC time?" — the LLM must call
   `current_time` because it has no real clock, proving the MCP server
   is actually invoked.

6. **Check MCP server logs for tool calls:**

   ```bash
   kubectl logs -l app=demo-mcp-server
   # [tools/call] current_time({})
   # [tools/call] -> {"content": [{"type": "text", "text": "2026-03-22T05:30:00Z"}], "isError": false}
   ```

7. **Test the MCP server directly from inside the cluster:**

   ```bash
   kubectl run mcptest --rm -it --image=curlimages/curl -- \
     curl -s http://demo-mcp-server.default.svc:3000 \
       -H 'Content-Type: application/json' \
       -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"current_time","arguments":{}}}'
   ```

### Cleanup

```bash
kubectl delete -f testdata/agents/mcpserver.yaml
```

The child Tool CRs are garbage collected automatically via owner references
when the MCPServer is deleted.
