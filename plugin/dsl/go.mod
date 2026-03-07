module github.com/alexedtionweb/niro-stream/plugin/dsl

go 1.23.0

require (
	github.com/alexedtionweb/niro-stream v0.0.0
	github.com/expr-lang/expr v1.16.1
)

require github.com/theory/jsonpath v0.11.0

replace github.com/alexedtionweb/niro-stream => ../..

replace github.com/alexedtionweb/niro-stream/plugin/agent => ../agent
