package memory

import (
	"context"
	"strings"
)

// Section is one block of a prompt.
// A nil Query is static: Write is called with no records. A nil Write renders
// records with Lines. An empty body is left out of the prompt.
type Section struct {
	Title string
	Query *Query
	Write func([]Record) string
}

// View is the system prompt: base instructions plus the sections, in order.
// Each section recalls its own query, so two models can read the same store
// through different views.
type View struct {
	Base     string
	Sections []Section
}

// Build recalls each section and joins the non-empty blocks.
// scope is written onto a query that does not set Scope or AnyScope.
func (v View) Build(ctx context.Context, store Store, scope string) (string, error) {
	var b strings.Builder
	if v.Base != "" {
		b.WriteString(v.Base)
	}
	for _, sec := range v.Sections {
		body, err := sec.render(ctx, store, scope)
		if err != nil {
			return "", err
		}
		if body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if sec.Title != "" {
			b.WriteString(sec.Title)
			b.WriteByte('\n')
		}
		b.WriteString(body)
	}
	return b.String(), nil
}

func (s Section) render(ctx context.Context, store Store, scope string) (string, error) {
	var recs []Record
	if s.Query != nil {
		if store == nil {
			return "", errNilStore
		}
		q := s.Query.scoped(scope)
		var err error
		recs, err = store.Recall(ctx, q)
		if err != nil {
			return "", err
		}
	}
	if s.Write != nil {
		return s.Write(recs), nil
	}
	return Lines(recs), nil
}

// Lines renders records as one line each: "source: body ref".
func Lines(records []Record) string {
	var b strings.Builder
	for i, r := range records {
		if i > 0 {
			b.WriteByte('\n')
		}
		if r.Source != "" {
			b.WriteString(r.Source)
			b.WriteString(": ")
		}
		b.WriteString(r.Body)
		if r.Ref != "" {
			if r.Body != "" {
				b.WriteByte(' ')
			}
			b.WriteString(r.Ref)
		}
	}
	return b.String()
}
