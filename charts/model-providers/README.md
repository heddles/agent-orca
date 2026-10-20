# model-providers

Ready-to-use ModelProvider CRs and credential Secrets for the most popular
cloud LLM providers: OpenAI, Anthropic, Google Gemini, Mistral, Cohere,
xAI (Grok), Groq, Together AI, and DeepSeek.

No infrastructure is deployed — only Secrets, ModelProvider CRs, and
ModelSelector CRs managed by the agent-orca operator.

## Usage

1. Enable the providers you want under `providerSecrets`.
2. Set `apiKey` for each provider (or point to an external secret).
3. Enable the individual model entries you want under `modelProviders`.
4. Enable and configure `modelSelectors` to route traffic across models.

```sh
helm install model-providers oci://ghcr.io/heddles/agent-orca/charts/model-providers \
  --version <version> -n agent-orca-system -f my-values.yaml
```
