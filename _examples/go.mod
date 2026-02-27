module ryn.dev/ryn/_examples

go 1.23.0

require (
	ryn.dev/ryn v0.0.0
	ryn.dev/ryn/provider/anthropic v0.0.0
	ryn.dev/ryn/provider/openai v0.0.0
)

require (
	github.com/anthropics/anthropic-sdk-go v1.26.0 // indirect
	github.com/openai/openai-go v0.1.0-beta.10 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/sync v0.16.0 // indirect
)

replace (
	ryn.dev/ryn => ..
	ryn.dev/ryn/provider/anthropic => ../provider/anthropic
	ryn.dev/ryn/provider/openai => ../provider/openai
)
