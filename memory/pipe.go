package memory

import "context"

// Stage rewrites a batch of records before they are stored.
// Return no records to drop the batch. Return more than one record from a
// single input to fan an event out into several memories.
type Stage func(ctx context.Context, in []Record) ([]Record, error)

// Pipe is an ordered list of stages. Pipes are values: Then returns a new pipe.
type Pipe struct {
	stages []Stage
}

// NewPipe returns a pipe that runs stages in order.
func NewPipe(stages ...Stage) *Pipe {
	return &Pipe{stages: append([]Stage(nil), stages...)}
}

// Then returns a pipe with more stages appended.
func (p *Pipe) Then(stages ...Stage) *Pipe {
	var base []Stage
	if p != nil {
		base = p.stages
	}
	next := make([]Stage, 0, len(base)+len(stages))
	next = append(next, base...)
	next = append(next, stages...)
	return &Pipe{stages: next}
}

// Filter keeps records for which fn returns true.
func Filter(fn func(Record) bool) Stage {
	return func(_ context.Context, in []Record) ([]Record, error) {
		if fn == nil {
			return in, nil
		}
		out := make([]Record, 0, len(in))
		for _, r := range in {
			if fn(r) {
				out = append(out, r)
			}
		}
		return out, nil
	}
}

// Map replaces each record with the records fn returns.
// An empty slice drops that record. An error stops the pipe before anything
// is stored.
func Map(fn func(context.Context, Record) ([]Record, error)) Stage {
	return func(ctx context.Context, in []Record) ([]Record, error) {
		if fn == nil {
			return in, nil
		}
		out := make([]Record, 0, len(in))
		for _, r := range in {
			next, err := fn(ctx, r)
			if err != nil {
				return nil, err
			}
			out = append(out, next...)
		}
		return out, nil
	}
}

// Commit runs the pipe, then stores whatever remains.
// A nil pipe stores the records as they are. A stage that returns nothing
// stops the pipe and stores nothing. A stage error stores nothing.
func (p *Pipe) Commit(ctx context.Context, store Store, recs ...Record) ([]Record, error) {
	if store == nil {
		return nil, errNilStore
	}
	if len(recs) == 0 {
		return nil, nil
	}
	batch := make([]Record, len(recs))
	for i, r := range recs {
		batch[i] = clone(r)
	}
	if p != nil {
		for _, stage := range p.stages {
			if stage == nil {
				continue
			}
			next, err := stage(ctx, batch)
			if err != nil {
				return nil, err
			}
			if len(next) == 0 {
				return nil, nil
			}
			batch = next
		}
	}
	out := make([]Record, 0, len(batch))
	for _, r := range batch {
		stored, err := store.Put(ctx, r)
		if err != nil {
			return out, err
		}
		out = append(out, stored)
	}
	return out, nil
}
