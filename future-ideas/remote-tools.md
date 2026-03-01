# Remote Tool Execution — WIP / Future Ideas

> **Status: Work In Progress — not yet scheduled for implementation.**
> This document captures the architectural direction for making tools a powerful,
> language-agnostic remote execution primitive with PCI-DSS compliance baked in.

---

## Motivation

Today `ToolHandler` is always an in-process Go function. The goal is to allow tools to
be execution targets that live anywhere — a Python sandbox, a Node.js MCP server, a Rust
binary behind gRPC — without changing how the LLM planner sees them. The declaration
plane (`ToolDefinition`) is already stable; what needs to change is the **execution plane**.

---

## Proposed Layers

### Layer 1 — `ToolExecutor` interface (the new contract)

Replace the bare `func(ctx, args) (any, error)` handler with a richer interface that
receives the full `niro.ToolCall` so remote systems can correlate, trace, and audit
individual invocations by call ID:

```go
type ToolExecutor interface {
    Execute(ctx context.Context, call niro.ToolCall) (niro.ToolResult, error)
}
```

`ToolDefinition` gains two new optional fields — fully backwards compatible; `Handler`
wins when `Executor` is nil:

```go
type ToolDefinition struct {
    Name, Description string
    Schema            json.RawMessage
    Handler           ToolHandler   // in-process (current, unchanged)
    Executor          ToolExecutor  // remote (new, takes priority when set)
    Tags              []string      // e.g. "pci-scoped", "destructive", "read-only"
    DataClass         DataClass     // Public | Internal | Confidential
}
```

---

### Layer 2 — `ExecutorRouter`

Sits between `Toolset.ExecuteCall` and the actual executor. Makes the final routing
decision based on tool metadata, never on call-site heuristics:

```go
type ExecutorRouter interface {
    Route(def ToolDefinition) (ToolExecutor, error)
}
```

Three planned built-in implementations:

| Router            | Logic                                                    |
| ----------------- | -------------------------------------------------------- |
| `StaticRouter`    | Explicit name → executor mapping                         |
| `DataClassRouter` | `Confidential` → PCI executor; others → general executor |
| `TagRouter`       | Route based on presence of a tag (e.g. `"pci-scoped"`)   |

---

### Layer 3 — gRPC Contract

A `.proto` definition gives language-agnostic, strongly-typed remote execution with
structured observability. Key design decisions:

- `call_id` maps directly to `niro.ToolCall.ID` — the correlation key across all logs
- `args_json` must never carry raw CHD (see Layer 5 — PCI)
- `ExecuteStream` enables incremental stdout/stderr from code runners
- `AuditRecord` is returned on every response, not just errors — required for PCI Req 10

```protobuf
syntax = "proto3";
package niro.executor.v1;

service ToolExecutorService {
    rpc Execute(ExecuteRequest)       returns (ExecuteResponse);
    rpc ExecuteStream(ExecuteRequest) returns (stream ExecuteEvent);   // for code sandboxes
    rpc ListTools(ListToolsRequest)   returns (ListToolsResponse);     // tool discovery
    rpc Check(HealthCheckRequest)     returns (HealthCheckResponse);   // circuit breaker
}

message ExecuteRequest {
    string call_id               = 1;  // niro.ToolCall.ID
    string tool_name             = 2;
    bytes  args_json             = 3;  // CHD must be tokenised before this is set
    string session_id            = 4;
    string tenant_id             = 5;
    map<string, string> metadata = 6;  // OTel baggage, auth headers, trace context
}

message ExecuteResponse {
    string call_id      = 1;
    bytes  result_json  = 2;
    bool   is_error     = 3;
    string error_code   = 4;  // "DENIED" | "TIMEOUT" | "EXEC_ERROR"
    AuditRecord audit   = 5;
}

message AuditRecord {
    string executor_id      = 1;
    string started_at       = 2;  // RFC3339 nanoseconds
    string completed_at     = 3;
    int64  duration_ms      = 4;
    bool   data_scrubbed    = 5;  // true when CHD was detected and replaced
    string scrubber_version = 6;
}
```

`ExecuteStream` events use `oneof { bytes stdout, bytes stderr, ExecuteResponse final }`
so code sandboxes can emit incremental output — critical for long-running Python/bash runs.

---

### Layer 4 — MCP Bridge

MCP (Model Context Protocol) is JSON-RPC 2.0 over stdio, SSE, or WebSocket. An
`MCPExecutor` provides pure protocol translation:

- `tools/list` → `[]ToolDefinition` for discovery at startup
- `tools/call` ← `niro.ToolCall` at execution time

This makes the entire Node.js / Python / Rust MCP ecosystem available as tools with
**zero Go code on the remote side**. Any process that speaks MCP becomes a tool server.

---

### Layer 5 — PCI-DSS Compliance

#### Root problem

The LLM's context _is the threat surface_. If a user types their card number and the
model generates `charge_card(pan="4111111111111111", ...)`, that PAN lives in `args_json`
of an `ExecuteRequest`. The moment it crosses the network to a remote sandbox, that
sandbox — and the transport — enter PCI scope. A general-purpose code sandbox that can
see real PANs becomes a card processor.

#### Core control — `DataScrubber`

The scrubber is **not a hook** (hooks observe) — it is a **mutation gate** that runs
before any args leave the trust boundary:

```go
type DataScrubber interface {
    // Scrub replaces CHD in args JSON with format-preserving tokens.
    // Returns scrubbed args, whether any hit occurred, and any error.
    Scrub(ctx context.Context, args json.RawMessage) (scrubbed json.RawMessage, hit bool, err error)
}
```

Implementation details:

- Luhn algorithm detects valid PANs (13–19 digit strings anywhere in the JSON)
- Format-preserving tokenisation: `4111111111111111` → `tok_411111xxxxxx1111`
  (first-6 / last-4 visible — useful for debugging without exposing the full PAN)
- Field-name heuristics: keys named `card_number`, `pan`, `cvv`, `cvc`, `expiry`
  trigger extra scrutiny regardless of value format
- Vault integration: real PANs stored in HashiCorp Vault / AWS Secrets Manager;
  only the token crosses the wire
- **Output scrubbing is mandatory**: scrubber also runs on all executor responses so
  a leaking sandbox stdout never reaches the LLM or conversation history

#### PCI-DSS requirement map

| Req       | Requirement                                       | Control in this stack                                                   |
| --------- | ------------------------------------------------- | ----------------------------------------------------------------------- |
| **3.4**   | Render PAN unreadable wherever stored/transmitted | `DataScrubber` wraps `ToolExecutor`; tokenise before transmission       |
| **4.2.1** | Strong cryptography in transit (TLS 1.2+)         | `GRPCExecutor` enforces mTLS — no plaintext option exposed in API       |
| **6.2**   | Input validation                                  | Already enforced in `Toolset.ExecuteCall` via JSON Schema               |
| **7.2**   | Least privilege access                            | `DataClassRouter` hard-routes `Confidential` tools to PCI executor only |
| **8**     | Authentication                                    | Client certificates on all gRPC connections                             |
| **10.2**  | Audit trail per call                              | `ToolRuntimeHook.OnToolExecuteEnd` → append-only audit sink             |
| **10.3**  | Tamper-evident logs                               | WORM / Merkle-chained log sink (hook implementation, not library)       |
| **12.3**  | Risk-ranked asset inventory                       | `DataClass` + `Tags` metadata on each `ToolDefinition`                  |

#### Code sandbox hard rules

A sandbox that can receive CHD **and** execute arbitrary code with it is the worst-case
scenario. Non-negotiable constraints:

1. `DataClass.Confidential` tools **must not** route to a general sandbox — `DataClassRouter`
   returns an error, not a warning or a log line.
2. Sandboxes must be **network-isolated** (gVisor, Firecracker, WASM) — no outbound
   networking so a Python `requests.post(pan_data)` cannot exfiltrate.
3. Output scrubbing is **mandatory**, not configurable-off for sandbox executors.

---

## Call Flow

```
                ┌── TRUST BOUNDARY ──────────────────────────────────┐
                │                                                      │
  User input ─► LLM ─► ToolApprover (HITL)                          │
                │              │                                       │
                │        DataScrubber ──── vault ◄── real PAN stored  │
                │          (tokenise args + outputs)                   │
                │              │                                       │
                │        ExecutorRouter                                │
                │          │           │                               │
                └──────────│───────────│───────────────────────────── ┘
                           │           │
               ┌───────────▼──┐   ┌───▼───────────────────┐
               │ GRPCExecutor  │   │ MCPExecutor             │
               │ (mTLS)        │   │ (stdio / SSE / WS)      │
               └───────────────┘   └─────────────────────────┘
                      │                        │
               Python sandbox           Node.js server
               Rust binary              Any MCP-compliant process
               Bash runner
```

---

## Planned Module Structure

Following the existing pattern (each heavy dependency gets its own `go.mod` to keep the
root module lean):

```
plugin/executor/
  grpc/        ← GRPCExecutor + generated proto client (grpc, protobuf deps)
    proto/
      executor.proto
    executor.go
  mcp/         ← MCPExecutor, JSON-RPC 2.0 client (no heavy deps)
    executor.go
  pci/         ← DataScrubber, Luhn check, format-preserving tokenisation
    scrubber.go
    luhn.go
  audit/       ← PCI-compliant ToolRuntimeHook → append-only audit log
    hook.go
```

---

## Implementation Order (when ready)

Dependencies flow top-to-bottom. Start here to avoid rework:

1. **`ToolExecutor` + `DataClass` + `Tags` on `ToolDefinition`** — core contract,
   backwards compatible, zero new dependencies
2. **`plugin/executor/pci` — `DataScrubber` + Luhn** — pure Go, no deps, the
   compliance foundation everything else builds on
3. **`ExecutorRouter`** — wires scrubber + routing into `Toolset`, still no network deps
4. **`plugin/executor/mcp`** — highest leverage: entire Python/Node.js MCP ecosystem
   becomes available immediately
5. **`plugin/executor/grpc`** — production-grade deployments; requires protobuf codegen
   and is the last piece since it depends on the stable proto contract above
