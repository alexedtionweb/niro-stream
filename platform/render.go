package platform

import (
	"bytes"
	"fmt"
	"text/template"
)

type TemplateRenderer struct{}

func (TemplateRenderer) Render(t Template, vars map[string]any) (string, error) {
	parsed, err := template.New(t.ID).Parse(t.Text)
	if err != nil {
		return "", fmt.Errorf("platform: parse template %q: %w", t.ID, err)
	}
	var buf bytes.Buffer
	if err := parsed.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("platform: execute template %q: %w", t.ID, err)
	}
	return buf.String(), nil
}
