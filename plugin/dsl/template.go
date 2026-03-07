package dsl

import (
	"strings"
	"sync"
	"text/template"

	"github.com/alexedtionweb/niro-stream"
)

var (
	tmplCache   = make(map[string]*template.Template)
	tmplCacheMu sync.RWMutex
)

// ExecuteTemplate parses the template (with caching) and executes it with data. Single entry point for template execution.
func ExecuteTemplate(templateStr string, data map[string]any) (string, error) {
	if templateStr == "" {
		return "", nil
	}
	t, err := getParsedTemplate(templateStr)
	if err != nil {
		return "", niro.WrapError(niro.ErrCodeInvalidRequest, "template parse", err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", niro.WrapError(niro.ErrCodeInvalidRequest, "template execute", err)
	}
	return buf.String(), nil
}

// ExpandTemplate executes the template with RunContext as data (prompts, tool descriptions).
func ExpandTemplate(templateStr string, runCtx *RunContext) (string, error) {
	return ExecuteTemplate(templateStr, runCtx.envMap())
}

// expandPrompt is ExpandTemplate for the system prompt.
func expandPrompt(templateStr string, runCtx *RunContext) (string, error) {
	return ExpandTemplate(templateStr, runCtx)
}

func getParsedTemplate(templateStr string) (*template.Template, error) {
	return GetOrCompute(&tmplCacheMu, &tmplCache, templateStr, func() (*template.Template, error) {
		return template.New("prompt").Parse(templateStr)
	})
}
