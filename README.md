# Completions API

A single-binary, OpenAI-compatible **completions gateway** with automatic provider
**failover**. It fronts a list of LLM providers and tries them **in order** — the
first one that succeeds wins, otherwise it moves down the list. A bundled web UI
lets you add/remove/reorder providers, issue/revoke **API keys**, and inspect
live usage.

All state (master password, provider configs, API keys, usage) is kept **in
memory** and is lost on restart.

## Providers

| Type | Protocol | Used for |
|------|----------|----------|
| `openai` | OpenAI-compatible `/chat/completions` (Bearer auth) | Hack Club AI, OpenRouter, any OpenAI-compatible server |
| `anthropic` | Anthropic Messages API (`x-api-key`) | Claude |
| `gemini` | Google `generateContent` (`x-goog-api-key`) | Gemini |

Hack Club AI (`https://ai.hackclub.com/proxy/v1`), OpenRouter
(`https://openrouter.ai/api/v1`), Anthropic, and Gemini presets are available in
the UI's "Add provider" dialog.

## Run

Requires Go 1.23+ (no third-party dependencies).

```bash
go run .
```

Or build a static binary:

```bash
go build -o completions-api .
./completions-api
```

On startup the server logs a **master password** (printed **once**, required to
open the admin dashboard) and an initial **API key** (required to call the
completions API).

```
==============================================================
  Completions API gateway

  Admin dashboard:     http://localhost:8080/
  Completions API:     POST http://localhost:8080/v1/chat/completions
  Models list:         GET  http://localhost:8080/v1/models
  API docs (llms.txt): GET  http://localhost:8080/llms.txt

  MASTER PASSWORD:     xxxx-yyyy-zzzz-wwww
  API key:             sk-gw-xxxx...
  (master password shown once; API keys are managed in the dashboard)
==============================================================
```

## Docker

```bash
docker build -t completions-api .
docker run -p 8080:8080 completions-api
```

The master password and initial API key appear in the container logs:

```bash
docker logs <container-id>
```

## Configure

Open `http://localhost:8080/`, sign in with the master password, then:

1. Create an **API key** (required to call the API).
2. Click **+ Add provider** and pick a preset (or enter a custom base URL).
3. Paste the provider API key. For aggregators (Hack Club AI, OpenRouter) leave **Model** blank (or check **Wildcard**) so any model is forwarded; for single-model providers set the model id.
4. Reorder providers with the ▲ / ▼ arrows — **order = failover priority**.
5. Use **Test** to verify a provider works, and toggle providers on/off.

## Use the API

It speaks the OpenAI Chat Completions protocol, so point any OpenAI SDK at it.
Every request must include an API key (a `Bearer` token or `x-api-key` header):

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello!"}]}'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-gw-...")
resp = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
```

Streaming is supported too (`stream=True`).

### Notes

- Each provider has an optional `model`. When blank the provider is a **wildcard**:
  the incoming request's `model` is forwarded as-is. Use a wildcard provider to
  front aggregators like Hack Club AI or OpenRouter with a single entry instead
  of one per model.
- If a provider has a fixed `model`, it always overrides the incoming request's
  `model`.
- **Streaming failover:** the gateway opens the upstream stream and checks the
  HTTP status before sending anything to the client. If the first provider
  returns a non-2xx it fails over to the next. Once a provider begins streaming
  (2xx), the gateway commits to it.
- `GET /v1/models` lists the models of enabled providers (useful for SDKs).

## Configuration (environment)

| Variable | Default | Meaning |
|----------|---------|---------|
| `PORT` | `8080` | HTTP listen port |
| `MASTER_PASSWORD` | random | Override the generated master password |
| `API_KEY` | random | Override the initial API key seeded at startup |
| `REQUEST_TIMEOUT_SECONDS` | `120` | Non-streaming upstream timeout |
| `STREAM_TIMEOUT_SECONDS` | `300` | Streaming upstream timeout |
| `SESSION_TTL_HOURS` | `24` | Admin session lifetime |
