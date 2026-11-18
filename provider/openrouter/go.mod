module github.com/alexedtionweb/niro-stream/provider/openrouter

go 1.26.10

require (
	github.com/OpenRouterTeam/go-sdk v0.9.116
	github.com/alexedtionweb/niro-stream v0.0.0
)

require github.com/spyzhov/ajson v0.9.6 // indirect

replace github.com/alexedtionweb/niro-stream => ../..
