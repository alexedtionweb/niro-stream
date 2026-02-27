// Tool calling example — LLM invokes tools via streaming.
//
// Demonstrates streaming tool calls: the LLM decides to call a tool,
// Ryn emits a ToolCallFrame, you execute the tool, and feed the result
// back for a final response.
//
// Usage:
//
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/tools
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"ryn.dev/ryn"
	"ryn.dev/ryn/provider/openai"
)

func main() {
	ctx := context.Background()

	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}

	llm := openai.New(key)

	// Define a tool
	weatherTool := ryn.Tool{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"city": {"type": "string", "description": "City name"}
			},
			"required": ["city"]
		}`),
	}

	// First request: let the LLM decide to call the tool
	messages := []ryn.Message{
		ryn.Text(ryn.RoleUser, "What's the weather in Tokyo?"),
	}

	stream, err := llm.Generate(ctx, &ryn.Request{
		Model:    "gpt-4o",
		Messages: messages,
		Tools:    []ryn.Tool{weatherTool},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Collect tool calls and text from the stream
	var toolCalls []*ryn.ToolCall
	for stream.Next(ctx) {
		f := stream.Frame()
		switch f.Kind {
		case ryn.KindText:
			fmt.Print(f.Text)
		case ryn.KindToolCall:
			fmt.Fprintf(os.Stderr, "[tool_call] %s(%s)\n", f.Tool.Name, f.Tool.Args)
			toolCalls = append(toolCalls, f.Tool)
		}
	}
	if err := stream.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
		os.Exit(1)
	}

	// If the LLM called tools, execute them and continue the conversation
	if len(toolCalls) > 0 {
		// Build assistant message with tool calls
		var assistantParts []ryn.Part
		for _, tc := range toolCalls {
			assistantParts = append(assistantParts, ryn.ToolCallPart(tc))
		}
		messages = append(messages, ryn.Multi(ryn.RoleAssistant, assistantParts...))

		// Execute each tool and add results
		for _, tc := range toolCalls {
			result := executeWeatherTool(tc)
			messages = append(messages, ryn.Multi(ryn.RoleTool, ryn.ToolResultPart(result)))
		}

		// Second request: get the final response with tool results
		stream, err = llm.Generate(ctx, &ryn.Request{
			Model:    "gpt-4o",
			Messages: messages,
			Tools:    []ryn.Tool{weatherTool},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

		for stream.Next(ctx) {
			f := stream.Frame()
			if f.Kind == ryn.KindText {
				fmt.Print(f.Text)
			}
		}
		fmt.Println()

		if err := stream.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
			os.Exit(1)
		}
	}
}

// executeWeatherTool simulates a tool execution.
func executeWeatherTool(tc *ryn.ToolCall) *ryn.ToolResult {
	var args struct {
		City string `json:"city"`
	}
	json.Unmarshal(tc.Args, &args)

	return &ryn.ToolResult{
		CallID:  tc.ID,
		Content: fmt.Sprintf(`{"city": %q, "temp_c": 22, "condition": "partly cloudy"}`, args.City),
	}
}
