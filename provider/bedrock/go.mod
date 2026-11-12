module github.com/alexedtionweb/niro-stream/provider/bedrock

go 1.24

require (
	github.com/alexedtionweb/niro-stream v0.0.0
	github.com/aws/aws-sdk-go-v2 v1.47.3
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.22
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.63.4
	github.com/aws/smithy-go v1.28.5
	github.com/google/uuid v1.6.0
)

require (
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.6 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.6 // indirect
)

replace github.com/alexedtionweb/niro-stream => ../..
