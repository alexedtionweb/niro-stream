module ryn.dev/ryn/provider/bedrock

go 1.23.0

require (
	github.com/aws/aws-sdk-go-v2 v1.41.2
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.5
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.50.0
	github.com/aws/smithy-go v1.24.1
	ryn.dev/ryn v0.0.0
)

require (
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.18 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.18 // indirect
)

replace ryn.dev/ryn => ../..
