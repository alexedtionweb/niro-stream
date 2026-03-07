package agent

import (
	"github.com/alexedtionweb/niro-stream"
)

// HistoryPolicy controls which conversation history is sent to the model on each turn.
// Attach with WithHistoryPolicy. Memory loads/saves history; the policy trims what is sent (and optionally what is saved).
//
// Optional: implement [HistorySavePolicy] to cap what is persisted.
// Optional: implement [BoundedHistoryPolicy] so the runtime can call [BoundedLoader.LoadLast] for better performance.
type HistoryPolicy interface {
	// TrimForRequest returns the history to include in the request (oldest to newest).
	// The runtime appends the new user message after this slice.
	TrimForRequest(history []niro.Message) []niro.Message
}

// BoundedHistoryPolicy is optional. When implemented, the runtime may call [BoundedLoader.LoadLast]
// with MaxMessages() so the backend can fetch only the last N messages (e.g. SQL LIMIT). Return 0 if unbounded.
type BoundedHistoryPolicy interface {
	HistoryPolicy
	MaxMessages() int // max messages this policy keeps; 0 = unknown/unbounded
}

// HistorySavePolicy is an optional extension: trim history before saving so storage stays bounded.
// If a HistoryPolicy also implements HistorySavePolicy, the runtime calls TrimForSave before Memory.Save.
type HistorySavePolicy interface {
	// TrimForSave returns the history to persist (e.g. last N messages). Return the slice unchanged to keep full history.
	TrimForSave(history []niro.Message) []niro.Message
}

// SlidingWindow keeps only the last maxMessages messages for the request (and for save, if used as HistorySavePolicy).
// Use to stay within context limits and optionally cap stored history. If maxMessages <= 0, no trimming is applied.
func SlidingWindow(maxMessages int) HistoryPolicy {
	return &slidingWindow{max: maxMessages}
}

type slidingWindow struct{ max int }

func (p *slidingWindow) TrimForRequest(history []niro.Message) []niro.Message {
	return p.trim(history)
}

func (p *slidingWindow) MaxMessages() int { return p.max }

func (p *slidingWindow) TrimForSave(history []niro.Message) []niro.Message {
	return p.trim(history)
}

func (p *slidingWindow) trim(history []niro.Message) []niro.Message {
	if p.max <= 0 || len(history) <= p.max {
		return history
	}
	return history[len(history)-p.max:]
}

// NoHistory drops all history; only the current user message is sent. Use for stateless or single-turn flows.
func NoHistory() HistoryPolicy {
	return noHistoryPolicy{}
}

type noHistoryPolicy struct{}

func (noHistoryPolicy) TrimForRequest(history []niro.Message) []niro.Message {
	return nil
}

func (noHistoryPolicy) MaxMessages() int { return 0 }

// KeepFirstAndLast keeps the first keepFirst and the last keepLast messages.
// Useful for "pinned" context (e.g. instructions or summary at the start) plus recent turns.
// If keepFirst or keepLast is 0, that end is not kept. Messages in the middle are dropped.
func KeepFirstAndLast(keepFirst, keepLast int) HistoryPolicy {
	return &keepFirstLast{first: keepFirst, last: keepLast}
}

type keepFirstLast struct{ first, last int }

func (p *keepFirstLast) TrimForRequest(history []niro.Message) []niro.Message {
	n := len(history)
	if n == 0 || (p.first <= 0 && p.last <= 0) {
		return history
	}
	if p.first <= 0 {
		if n <= p.last {
			return history
		}
		return history[n-p.last:]
	}
	if p.last <= 0 {
		if n <= p.first {
			return history
		}
		return history[:p.first]
	}
	if n <= p.first+p.last {
		return history
	}
	out := make([]niro.Message, 0, p.first+p.last)
	out = append(out, history[:p.first]...)
	out = append(out, history[n-p.last:]...)
	return out
}

func (p *keepFirstLast) MaxMessages() int { return p.first + p.last }

// TokenCounter returns the approximate token count for a slice of messages.
// Used by SlidingWindowTokens to trim history to stay under a token budget.
type TokenCounter func(msgs []niro.Message) int

// SlidingWindowTokens keeps as many trailing messages as fit within maxTokens (according to counter).
// Trims from the front until the remaining messages have total tokens <= maxTokens.
// If maxTokens <= 0 or counter is nil, no trimming is applied.
func SlidingWindowTokens(maxTokens int, counter TokenCounter) HistoryPolicy {
	return &slidingWindowTokens{max: maxTokens, count: counter}
}

type slidingWindowTokens struct {
	max   int
	count TokenCounter
}

func (p *slidingWindowTokens) TrimForRequest(history []niro.Message) []niro.Message {
	if p.max <= 0 || p.count == nil || len(history) == 0 {
		return history
	}
	if p.count(history) <= p.max {
		return history
	}
	// Trim from the front until we're under the budget.
	for i := 1; i < len(history); i++ {
		trimmed := history[i:]
		if p.count(trimmed) <= p.max {
			return trimmed
		}
	}
	// Even the last message exceeds budget; return it anyway so we send something.
	return history[len(history)-1:]
}

func (p *slidingWindowTokens) MaxMessages() int { return 0 } // token-based, no fixed message cap

// Chain applies policies in order: first policy trims, then the next trims that result, and so on.
// Use to combine e.g. KeepFirstAndLast(2, 0) with SlidingWindow(20) so the final request has at most 20 messages after keeping the first 2.
func Chain(policies ...HistoryPolicy) HistoryPolicy {
	if len(policies) == 0 {
		return NoHistory()
	}
	if len(policies) == 1 {
		return policies[0]
	}
	return &chainPolicy{policies: policies}
}

type chainPolicy struct {
	policies []HistoryPolicy
}

func (c *chainPolicy) TrimForRequest(history []niro.Message) []niro.Message {
	for _, p := range c.policies {
		history = p.TrimForRequest(history)
	}
	return history
}

func (c *chainPolicy) MaxMessages() int {
	min := 0
	for _, p := range c.policies {
		if b, ok := p.(BoundedHistoryPolicy); ok {
			n := b.MaxMessages()
			if n <= 0 {
				return 0
			}
			if min == 0 || n < min {
				min = n
			}
		} else {
			return 0
		}
	}
	return min
}
