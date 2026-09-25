# Adding a model provider

> The whole platform talks to a model through one three-method interface. A
> provider adapter turns that interface into one vendor's API and classifies
> its errors; the gateway around it (quota, retries, fallback, cost) does not
> change. The PoC ships a scripted `FakeProvider`; this guide is what a real
> one needs.
>
> Code: [`backend/internal/llm/llm.go`](../../backend/internal/llm/llm.go), [`gateway.go`](../../backend/internal/llm/gateway.go), [`fake.go`](../../backend/internal/llm/fake.go) · Related: [model plane](../architecture/06-model-plane.md) · [LLM integration reasoning](../reasoning/09-llm-integration.md)

## 1. The contract

```go
type Provider interface {
    Name() string
    Complete(ctx context.Context, req Request) (Response, error)
}

// Request  — Model, System, Messages ([]Message{Role, Content, ToolCalls, ToolCallID, IsError}),
//            Tools ([]tools.Schema: name, description, parameters, required), MaxTokens
// Response — Content, ToolCalls ([]ToolCall{ID, Name, Args}), StopReason ("end_turn" | "tool_use" |
//            "human_input_required"), InputTokens, OutputTokens, Model, CostUSD (may be left 0: the gateway prices it)
```

Obligations of `Complete`:

| Obligation | Why |
|---|---|
| honour `ctx` (deadline, cancel) | the worker's lease context cancels it when the lease is lost |
| return `llm.ErrRateLimited` (wrapped) on 429 and `llm.ErrOverloaded` on 529/503-overloaded | these are the **only** retryable classes; anything else fails the call and the run's step is told why |
| fill `InputTokens`/`OutputTokens` from the vendor's usage block | `Settle()` uses them; zero means the tenant is refunded the whole estimate |
| map the vendor's tool-call structure to `ToolCalls` with stable `ID`s | the worker writes `TOOL_CALL`/`TOOL_RESULT` keyed by `tool_call_id` |
| map "the model wants a human" to `StopReason = "human_input_required"` | that string parks the run in `WAITING_HUMAN` |
| never log the request body | it contains tenant data; log ids, sizes and latency |
| send **only** `req.Tools` schemas | never a backend detail; the registry already strips them, do not re-add |

## 2. A minimal adapter skeleton

```go
type anthropicProvider struct{ key creds.Secret; base string; http *http.Client }

func (p *anthropicProvider) Name() string { return "anthropic" }

func (p *anthropicProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
    body := toMessagesAPI(req)                       // system, messages, tools, max_tokens
    hreq, _ := http.NewRequestWithContext(ctx, "POST", p.base+"/v1/messages", body)
    hreq.Header.Set("x-api-key", p.key.Reveal())     // ← a NEW Reveal() site: the count test must be updated in the same PR
    hreq.Header.Set("anthropic-version", "2023-06-01")
    resp, err := p.http.Do(hreq)
    if err != nil { return llm.Response{}, err }      // network error: not retryable by class → the run requeues via the outer path
    defer resp.Body.Close()
    switch resp.StatusCode {
    case 429: return llm.Response{}, fmt.Errorf("anthropic: %w", llm.ErrRateLimited)
    case 529, 503: return llm.Response{}, fmt.Errorf("anthropic: %w", llm.ErrOverloaded)
    }
    if resp.StatusCode/100 != 2 { return llm.Response{}, fmt.Errorf("anthropic: status %d", resp.StatusCode) }
    return fromMessagesAPI(resp.Body)                 // content blocks → Content + ToolCalls; usage → tokens; stop_reason
}
```

Put the API key in a `creds.Secret` so it cannot be logged; read it from a
Kubernetes Secret via env (`ANTHROPIC_API_KEY`), never from a flag.

## 3. Price table

Add the model to `llm.priceTable` (`$/Mtok` in and out). An unknown model is
priced at the **most expensive known rate** on purpose — under-reporting spend
is the failure nobody investigates. Cached-input pricing is a column to add
when the adapter reports cache tokens.

## 4. Wiring

`serve.go` constructs `llm.NewFakeProvider(...)` and passes it to
`llm.NewGateway(provider, limiter, metrics, log)`. Replace it behind a flag:

```go
switch o.Provider {                       // --provider / AGENTORCH_PROVIDER: fake | anthropic | …
case "anthropic": primary = newAnthropic(secretFromEnv("ANTHROPIC_API_KEY"))
default:          primary = llm.NewFakeProvider(o.ModelLatency, o.ModelLatency/2)
}
modelGW := llm.NewGateway(primary, limiter, metrics, log).WithFallback(optionalSecondary)
```

The flag does not exist yet; adding it is a documented production step. The
fake provider stays available for `make demo` and every test.

## 5. Tests

| Test | Proves |
|---|---|
| request mapping | system prompt, messages, tool schemas and `max_tokens` land in the vendor body; **no** backend fields |
| response mapping | content, tool calls with ids, stop reason, token counts |
| error classification | 429 → `ErrRateLimited`, 529 → `ErrOverloaded`, 400 → plain error (not retried) |
| gateway integration | with a stub server: 3 attempts with backoff on 429; fallback on the third; `Settle` exactly once |
| no secret in logs | run with a logging hook and assert the key never appears |

Record/replay fixtures (a saved vendor response per scenario) keep these
offline and deterministic; a live smoke test behind an env var is the only
network test.

## 6. Operating it

* Set `--provider-tpm` slightly below the vendor's contractual tokens/minute
  for the org; the limiter does the rest.
* Watch `agentorch_model_calls_total`, `_model_tokens_total`,
  `_model_cost_usd_total` per tenant and model; reconcile monthly against the
  invoice — a drift means the price table or the usage mapping is wrong.
* A vendor incident shows up as runs parked with `status_reason: model
  provider unavailable`, not as failures; nothing to do but wait or switch
  the fallback.
