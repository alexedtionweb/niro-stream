package memory

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/alexedtionweb/niro-stream"
)

// Store is the memory backend.
// Put with an existing ID in the same scope replaces that record.
// Recall returns matches oldest first. Forget removes matches.
type Store interface {
	Put(ctx context.Context, rec Record) (Record, error)
	Recall(ctx context.Context, q Query) ([]Record, error)
	Forget(ctx context.Context, q Query) (int, error)
}

// Mem is an in-process store. cap is the most records kept per scope;
// zero keeps them all. The oldest record in that scope is dropped past the cap.
type Mem struct {
	mu   sync.Mutex
	cap  int
	seq  uint64
	recs []Record
}

// New returns an in-process store.
func New(cap int) *Mem {
	return &Mem{cap: cap}
}

// Put stores a copy of rec. At is set to now when it is zero.
// An empty Body and Ref is rejected. A new record with an empty ID gets one.
func (m *Mem) Put(_ context.Context, rec Record) (Record, error) {
	if m == nil {
		return Record{}, niro.NewError(niro.ErrCodeInvalidRequest, "memory store is nil")
	}
	if rec.Body == "" && rec.Ref == "" {
		return Record{}, niro.NewError(niro.ErrCodeInvalidRequest, "record is empty")
	}
	rec = clone(rec)
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec.ID == "" {
		m.seq++
		rec.ID = strconv.FormatUint(m.seq, 10)
	} else {
		m.dropID(rec.Scope, rec.ID)
	}
	m.recs = append(m.recs, rec)
	m.trim(rec.Scope)
	return clone(rec), nil
}

// Recall returns copies of the matching records, oldest first.
func (m *Mem) Recall(_ context.Context, q Query) ([]Record, error) {
	if m == nil {
		return nil, niro.NewError(niro.ErrCodeInvalidRequest, "memory store is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	matched := make([]Record, 0)
	for _, r := range m.recs {
		if q.Allows(r) {
			matched = append(matched, clone(r))
		}
	}
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[len(matched)-q.Limit:]
	}
	return matched, nil
}

// Forget removes matching records and returns how many were removed.
func (m *Mem) Forget(_ context.Context, q Query) (int, error) {
	if m == nil {
		return 0, niro.NewError(niro.ErrCodeInvalidRequest, "memory store is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := make([]Record, 0, len(m.recs))
	n := 0
	for _, r := range m.recs {
		if q.Allows(r) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.recs = kept
	return n, nil
}

func (m *Mem) dropID(scope, id string) {
	kept := m.recs[:0]
	for _, r := range m.recs {
		if r.Scope == scope && r.ID == id {
			continue
		}
		kept = append(kept, r)
	}
	m.recs = kept
}

func (m *Mem) trim(scope string) {
	if m.cap <= 0 {
		return
	}
	n := 0
	for _, r := range m.recs {
		if r.Scope == scope {
			n++
		}
	}
	for n > m.cap {
		for i, r := range m.recs {
			if r.Scope == scope {
				m.recs = append(m.recs[:i], m.recs[i+1:]...)
				n--
				break
			}
		}
	}
}
