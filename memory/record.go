// Package memory ingests records, builds a prompt from them, and runs that
// prompt on many models at once.
//
// A record is text plus a pointer. Media bytes and sockets stay outside.
// Compose a Pipe to decide what is stored, a View to decide what each model
// reads, and a Plan to run those models together or take the first result.
package memory

import "time"

// Record is one thing worth remembering.
// Ref points at something stored elsewhere (a URL, an id). A target with See
// resolves Ref through an Album into an image or video part. Tags and Meta
// are for the caller; this package does not interpret them.
type Record struct {
	ID     string
	Scope  string
	Source string
	Body   string
	Ref    string
	Tags   []string
	Meta   map[string]string
	At     time.Time
}

// Query selects records. Zero fields match nothing extra: an empty Query
// matches only records whose Scope is empty.
//
// Sources match if the record source is one of them. Tags match only when
// the record carries every listed tag. Until is exclusive. Limit is applied
// by the store after matching, keeping the newest records. Limit is not part
// of Allows.
type Query struct {
	ID       string
	Scope    string
	AnyScope bool
	Sources  []string
	Tags     []string
	Since    time.Time
	Until    time.Time
	Limit    int
	Match    func(Record) bool
}

// Allows reports whether r matches the query predicate.
func (q Query) Allows(r Record) bool {
	if q.ID != "" && r.ID != q.ID {
		return false
	}
	if !q.AnyScope && r.Scope != q.Scope {
		return false
	}
	if len(q.Sources) > 0 && !contains(q.Sources, r.Source) {
		return false
	}
	if len(q.Tags) > 0 && !hasAll(r.Tags, q.Tags) {
		return false
	}
	if !q.Since.IsZero() && r.At.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && !r.At.Before(q.Until) {
		return false
	}
	if q.Match != nil && !q.Match(r) {
		return false
	}
	return true
}

// scoped fills Scope from the caller when this query does not name one.
func (q Query) scoped(scope string) Query {
	if q.Scope == "" && !q.AnyScope {
		q.Scope = scope
	}
	return q
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func hasAll(have, need []string) bool {
	for _, n := range need {
		if !contains(have, n) {
			return false
		}
	}
	return true
}

func clone(r Record) Record {
	if r.Tags != nil {
		r.Tags = append([]string(nil), r.Tags...)
	}
	if r.Meta != nil {
		meta := make(map[string]string, len(r.Meta))
		for k, v := range r.Meta {
			meta[k] = v
		}
		r.Meta = meta
	}
	return r
}
