# niro examples

Runnable examples for [niro](https://pkg.go.dev/github.com/alexedtionweb/niro-stream) — a provider-agnostic Go library for streaming LLM generation.

Every example selects a provider at runtime via environment variables, so you
can swap backends without editing code.

---

## Provider quick-start

| Provider           | Env vars needed                | Notes                                                                |
| ------------------ | ------------------------------ | -------------------------------------------------------------------- |
| **OpenAI**         | `OPENAI_API_KEY`               | Default in most examples                                             |
| **Anthropic**      | `ANTHROPIC_API_KEY`            |                                                                      |
| **Google Gemini**  | `GEMINI_API_KEY`               | Get one at [aistudio.google.com](https://aistudio.google.com/apikey) |
| **Amazon Bedrock** | `AWS_REGION` + AWS credentials | See [credential chain](#bedrock-credentials)                         |
| **Ollama (local)** | _(none)_                       | Ollama must be running on `localhost:11434`                          |
| **ElevenLabs**     | `ELEVENLABS_API_KEY`           | Used by the `elevenlabs` demo (TTS + STT)                             |

### Bedrock credentials

Bedrock uses the standard AWS credential chain — whichever is found first wins:

```
AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ AWS_SESSION_TOKEN)
~/.aws/credentials / ~/.aws/config
IAM instance role / ECS task role / EKS pod identity
```

The default model is `anthropic.claude-3-5-sonnet-20241022-v2:0`.  
Override it with `MODEL=amazon.nova-pro-v1:0` (or any Bedrock model ID you have access to).

---

## Examples

### `chat` — streaming chat, all providers

The simplest entry point. Streams a single response and prints it token by
token. Supports all five providers via the `PROVIDER` env var.

```bash
# OpenAI (default)
OPENAI_API_KEY=sk-...  go run ./chat

# Anthropic
ANTHROPIC_API_KEY=sk-ant-...  PROVIDER=anthropic  go run ./chat

# Google Gemini
GEMINI_API_KEY=...  PROVIDER=gemini  go run ./chat

# Amazon Bedrock (Claude 3.5 Sonnet)
AWS_REGION=us-east-1  PROVIDER=bedrock  go run ./chat

# Amazon Bedrock with a different model
AWS_REGION=us-east-1  PROVIDER=bedrock  MODEL=amazon.nova-pro-v1:0  go run ./chat

# Ollama (llama3.2 by default)
PROVIDER=ollama  go run ./chat

# Custom prompt
OPENAI_API_KEY=sk-...  PROMPT="Explain monads in one sentence."  go run ./chat
```

---

### `gemini` — Gemini-specific features

Demonstrates three Gemini-specific patterns using `github.com/alexedtionweb/niro-stream/provider/google`:

1. **Basic streaming** — token-by-token output with `gemini-2.0-flash`
2. **Structured JSON output** — `structured.GenerateStructured[T]` decodes the
   response directly into a typed Go struct (a `Recipe`)
3. **Multi-turn conversation** — appends assistant replies to the message
   history and continues the chat

```bash
GEMINI_API_KEY=...  go run ./gemini
```

---

### `bedrock` — Amazon Bedrock features

Demonstrates three Bedrock-specific patterns using `github.com/alexedtionweb/niro-stream/provider/bedrock`:

1. **Basic streaming chat** — single-turn response
2. **Multi-turn conversation** — serverless → containers → comparison
3. **Tool calling** — stock price lookup with automatic round-trip handling via
   `tools.NewToolLoop`

```bash
AWS_REGION=us-east-1  go run ./bedrock

# Use Amazon Nova Pro instead
MODEL=amazon.nova-pro-v1:0  AWS_REGION=us-east-1  go run ./bedrock
```

---

### `multi-provider` — runtime provider routing

Shows how to register multiple providers in a `registry.Registry` and route
requests at runtime using `registry.NewMultiTenantProvider`. Only providers
whose credentials are present are registered, so the binary works even if some
keys are missing.

```bash
# Route all requests to Gemini
GEMINI_API_KEY=...  PROVIDER=gemini  go run ./multi-provider

# Mix: register all four, route each in turn
OPENAI_API_KEY=sk-...  ANTHROPIC_API_KEY=sk-ant-...  \
  GEMINI_API_KEY=...  AWS_REGION=us-east-1            \
  go run ./multi-provider
```

Key patterns shown:

```go
reg := registry.New()
reg.Register("openai",    openai.New(key))
reg.Register("gemini",    googleProvider)
reg.Register("bedrock",   bedrock.New(awsCfg))

router := registry.NewMultiTenantProvider(reg,
    registry.WithDefaultClient("openai"),
    registry.WithClientSelector(func(ctx context.Context, req *niro.Request) (string, error) {
        return os.Getenv("PROVIDER"), nil   // env var overrides
    }),
    registry.WithClientMutator("anthropic", func(ctx context.Context, req *niro.Request) error {
        req.Model = "claude-3-5-sonnet-20241022"   // per-provider defaults
        return nil
    }),
)

// req.Client overrides the selector for a specific call
stream, err := router.Generate(ctx, &niro.Request{Client: "gemini", ...})
```

---

### `parallel` — Fan / Race / Sequence orchestration

Demonstrates the three orchestration primitives in `github.com/alexedtionweb/niro-stream/orchestrate`:

| Pattern      | What it does                                                    |
| ------------ | --------------------------------------------------------------- |
| **Fan**      | Fires N requests in parallel, merges all streams in order       |
| **Race**     | Fires N requests in parallel, returns the first to complete     |
| **Sequence** | Chains steps — each step receives the previous step's full text |

```bash
OPENAI_API_KEY=sk-...  go run ./parallel
```

```go
// Fan: three questions answered in parallel
stream := orchestrate.Fan(ctx, gen("What is Go?"), gen("What is Rust?"), gen("What is Zig?"))

// Race: three sampling temperatures, fastest wins
text, usage, err := orchestrate.Race(ctx, gen(0.1), gen(0.5), gen(0.9))

// Sequence: haiku → critique (output of step 1 becomes input of step 2)
stream, err := orchestrate.Sequence(ctx, writeHaiku, critiqueHaiku)
```

---

### `pipeline` — frame processing and hooks

Shows how to attach a multi-stage `pipe.Pipeline` and a `hook.Hook` to a
`runtime.Runtime`:

```
Generate → TextOnly → Map(toUpper) → Accumulate → single final frame
                                               ↑ Hook logs timing to stderr
```

```bash
OPENAI_API_KEY=sk-...  go run ./pipeline
```

Useful for:

- Filtering frames (strip non-text frames with `pipe.TextOnly`)
- Transforming tokens (`pipe.Map`)
- Collapsing a stream into one frame (`pipe.Accumulate`)
- Observability — the hook receives `OnGenerateStart`, `OnGenerateEnd`, and
  `OnError` events with model name, token counts, and wall-clock duration

---

### `tools` — automatic tool-call loop

Shows `tools.NewToolLoop` which handles the full multi-round
`generate → parse tool calls → execute → append results → generate` cycle
automatically. Two tools are registered: `get_weather` and `get_current_time`.

```bash
OPENAI_API_KEY=sk-...  go run ./tools
```

```go
loop := tools.NewToolLoop(ts, 5 /* maxRounds */)
stream, err := loop.GenerateWithTools(ctx, llm, req)
// stream contains only the final text response — tool round-trips are hidden
```

---

### `elevenlabs` — TTS + STT (speech)

Demonstrates:

- **TTS**: synthesize text into an audio file (streams audio frames)
- **STT (batch)**: transcribe an audio file into text (HTTP)
- **STT (realtime)**: transcribe streamed audio frames into text (WebSocket)

```bash
ELEVENLABS_API_KEY=...  go run ./elevenlabs tts -text "Hello from Niro" -out hello.mp3
ELEVENLABS_API_KEY=...  go run ./elevenlabs stt -in sample.wav
ELEVENLABS_API_KEY=...  go run ./elevenlabs stt-stream -in sample.pcm
```

For `stt-stream`, the recommended input is **raw PCM** (`s16le`, mono, 16kHz).
Example conversion:

```bash
ffmpeg -i input.wav -f s16le -ac 1 -ar 16000 sample.pcm
```

---

## Running all examples

```bash
# Clone and enter the repo
git clone <repo-url> && cd go-llm

# Run any example (replace placeholders)
OPENAI_API_KEY=sk-...  go run ./examples/chat
GEMINI_API_KEY=...     go run ./examples/gemini
AWS_REGION=us-east-1   go run ./examples/bedrock
```

All examples live in a single Go module (`github.com/alexedtionweb/niro-stream/examples`) inside a
Go workspace, so no extra setup is required beyond setting the relevant
environment variables.
