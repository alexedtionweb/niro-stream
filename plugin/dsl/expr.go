package dsl

import (
	"sync"

	"github.com/alexedtionweb/niro-stream"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

const responseEnvCacheKeyPrefix = "\x00response:"

var (
	exprCache   = make(map[string]*vm.Program)
	exprCacheMu sync.RWMutex
)

// compileAndRunExpr compiles the expression (with caching) and runs it with the given env.
// Used for validation (stub env) and runtime (RunContext as env).
func compileAndRunExpr(exprStr string, env map[string]any) (any, error) {
	if exprStr == "" {
		return nil, nil
	}
	program, err := getCompiledExpr(exprStr)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "expr compile", err)
	}
	out, err := expr.Run(program, env)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "expr run", err)
	}
	return out, nil
}

// getCompiledExpr returns a cached compiled program for the expression, or compiles and caches it.
func getCompiledExpr(exprStr string) (*vm.Program, error) {
	return GetOrCompute(&exprCacheMu, &exprCache, exprStr, func() (*vm.Program, error) {
		return expr.Compile(exprStr, expr.Env(runContextExprEnv()))
	})
}

func runContextExprEnv() map[string]any {
	return map[string]any{"session": map[string]any{}, "event": map[string]any{}, "history": ""}
}

// CompileRunContextExpr compiles a when/unless expression; use from validation so env matches runtime.
func CompileRunContextExpr(exprStr string) error {
	_, err := getCompiledExpr(exprStr)
	if err != nil {
		return niro.WrapError(niro.ErrCodeInvalidRequest, "expr compile", err)
	}
	return nil
}

// EvalCondition evaluates a when/unless expression with the given RunContext as env.
// Returns true if the condition passes (for "when": include tool; for "unless": exclude if true).
// If the expression returns a bool, that value is used; otherwise the result is treated as truthy
// (e.g. non-empty string, non-nil, non-zero number) so conditions like "event.text" work.
func EvalCondition(exprStr string, runCtx *RunContext) (bool, error) {
	if exprStr == "" {
		return true, nil
	}
	env := runCtx.envMap()
	out, err := compileAndRunExpr(exprStr, env)
	if err != nil {
		return false, err
	}
	if b, ok := out.(bool); ok {
		return b, nil
	}
	return truthy(out), nil
}

// truthy returns true for values that are considered truthy (non-empty string, non-nil, non-zero number).
func truthy(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// EvalResponseCondition compiles and runs an expression with env {"$": response}.
// Used for HTTP tool cases: when is evaluated against response (status, body).
func EvalResponseCondition(exprStr string, response map[string]any) (bool, error) {
	if exprStr == "" {
		return true, nil
	}
	program, err := getCompiledExprForResponse(exprStr)
	if err != nil {
		return false, niro.WrapError(niro.ErrCodeInvalidRequest, "expr response", err)
	}
	env := map[string]any{"$": response}
	out, err := expr.Run(program, env)
	if err != nil {
		return false, niro.WrapError(niro.ErrCodeInvalidRequest, "expr response run", err)
	}
	b, _ := out.(bool)
	return b, nil
}

func getCompiledExprForResponse(exprStr string) (*vm.Program, error) {
	key := responseEnvCacheKeyPrefix + exprStr
	return GetOrCompute(&exprCacheMu, &exprCache, key, func() (*vm.Program, error) {
		env := map[string]any{
			"$": map[string]any{
				"status": 0,
				"body":   map[string]any{},
			},
		}
		return expr.Compile(exprStr, expr.Env(env))
	})
}
