// Package agent provides an optional runtime layer for conversational agents:
// provider, memory (history), system prompt, tools, and plugin components.
//
// # History and storage
//
// Conversation history is resolved and persisted via the [Memory] interface.
// Implement Memory to inject any storage backend; the runtime only calls Load and Save.
//
//   - No storage (stateless): omit [WithMemory] or use [StatelessMemory].
//   - In-process: [NewInMemoryMemory] for local/dev.
//   - SQL: implement Memory; Load loads messages for session (e.g. JSON column or rows); Save upserts.
//   - NoSQL: one document per session with a messages array; Load/Save read/write that document.
//   - Redis: session key → serialized []niro.Message (e.g. JSON); set TTL if needed.
//
// [HistoryPolicy] (and optional [HistorySavePolicy]) control what is sent to the model
// and what is persisted; they work with any Memory implementation.
//
// # Performance
//
// When [Memory] implements [BoundedLoader] and [HistoryPolicy] implements [BoundedHistoryPolicy]
// with MaxMessages() > 0 (e.g. [SlidingWindow](20)), the runtime calls LoadLast(sessionID, max)
// so the backend can fetch only the last N messages (e.g. SQL ORDER BY id DESC LIMIT N, Redis LRANGE).
// Otherwise the runtime uses Load and trims in memory.
//
// # Fault tolerance
//
// The runtime retries Load and Save with exponential backoff (see [WithMemoryRetry]; default 3 attempts,
// 50ms initial backoff). Implementers should make Save idempotent (same input → same stored state)
// so retries are safe.
package agent
