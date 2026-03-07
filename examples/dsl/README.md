# DSL chat example

Interactive chat driven by the agent DSL: agents and workflow are defined in JSON; tools can be HTTP (e.g. ViaCEP) or code-based.

## Config files

- **agents.json** — tools and agents. HTTP tools (`type: "http"`) support:
  - **output**: `"text"` = return raw response body as string; `"json"` or omit = use **cases** (pick/parse JSON with field mapping) or decode JSON. Use **cases** with `when` (expr over `$.status`, `$.body`) and `parse` (JSONPath e.g. `$.body.cep`, literals `'value'`) to shape the tool result.
  - Each tool ref can have:
  - **when** (expr): include the tool only when this expression is true (e.g. `event.text` — non-empty string is truthy in expr; or `session.allow_tools`).
  - **unless** (expr): exclude the tool when this expression is true (e.g. `session.disable_cep == true`).
  Expressions are evaluated with [expr](https://github.com/expr-lang/expr) against the **RunContext** (`session`, `event`, `history`, and any custom vars). The chat example sets `event.text` to the current user message.
- **workflow.json** — workflow definitions: **fan** (parallel merge), **race** (first wins), **sequence** (chained), **fan_then** (parallel then synthesizer).

  The example defines **chat** (classifier → handoff to address_agent, general_agent, or **debate**) and **debate** (optimist + skeptic in parallel, then synthesizer). The classifier routes to debate for advice/decision questions; you see all three outputs (Optimist, Skeptic, Conclusion).

## Run

From repo root or from this directory:

```bash
PROVIDER=gemini GEMINI_API_KEY=... go run ./examples/dsl
# or
cd examples/dsl && PROVIDER=openai OPENAI_API_KEY=... go run .
```

Commands: `/help`, `/clear`, `/history`, `/usage`, `/quit`.
