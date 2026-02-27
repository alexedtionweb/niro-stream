// Command tools demonstrates tool calling with automatic round-trip.
//
// Usage:
//
//	OPENAI_API_KEY=sk-... go run ./examples/tools
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"ryn.dev/ryn"
	"ryn.dev/ryn/provider/openai"
)

func main() {
	ctx := context.Background()

	llm := openai.New(os.Getenv("OPENAI_API_KEY"))

	// Define a weather tool
	weatherTool := ryn.Tool{
		Name:        "get_weather",
		Description: "Get the current weather for a city",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"city": {"type": "string", "description": "City name"}
			},
			"required": ["city"]
		}`),
	}

	// First turn: ask about weather
	messages := []ryn.Message{
		ryn.UserText("What's the weather in San Francisco and Tokyo?"),
	}

	fmt.Println("User: What's the weather in San Francisco and Tokyo?")
	fmt.Println()

	// Tool loop — keep generating until we get a non-tool-call response
	for {
		stream, err := llm.Generate(ctx, &ryn.Request{
			Model:    "gpt-4o",
			Messages: messages,
			Tools:    []ryn.Tool{weatherTool},
			Options:  ryn.Options{MaxTokens: 512},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate: %v\n", err)
			os.Exit(1)
		}

		// Collect all frames
		var (
			textBuf   []byte
			toolCalls []*ryn.ToolCall
		)

		for stream.Next(ctx) {
			f := stream.Frame()
			switch f.Kind {
			case ryn.KindText:
				fmt.Print(f.Text)
				textBuf = append(textBuf, f.Text...)
			case ryn.KindToolCall:
				toolCalls = append(toolCalls, f.Tool)
				fmt.Fprintf(os.Stderr, "\n[Tool Call] %s(%s)\n", f.Tool.Name, f.Tool.Args)
			}
		}
		if err := stream.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "stream: %v\n", err)
			os.Exit(1)
		}

		// No tool calls → done
		if len(toolCalls) == 0 {
			fmt.Println()
			break
		}

		// Build assistant message with both text and tool calls
		var parts []ryn.Part
		if len(textBuf) > 0 {
			parts = append(parts, ryn.TextPart(string(textBuf)))
		}
		for _, tc := range toolCalls {
			parts = append(parts, ryn.ToolCallPart(tc))
		}
		messages = append(messages, ryn.Multi(ryn.RoleAssistant, parts...))

		// Execute tool calls and add results
		for _, tc := range toolCalls {
			result := executeTool(tc)
			fmt.Fprintf(os.Stderr, "[Tool Result] %s → %s\n", tc.Name, result)
			messages = append(messages, ryn.ToolMessage(tc.ID, result))
		}
	}

	fmt.Fprintf(os.Stderr, "\nDone.\n")
}

// executeTool simulates tool execution
func executeTool(tc *ryn.ToolCall) string {
	// Simulate network delay
	time.Sleep(50 * time.Millisecond)

	var args struct {
		City string `json:"city"`
	}
	json.Unmarshal(tc.Args, &args)

	// Fake weather data
	weather := map[string]string{
		"San Francisco": `{"temp": 68, "condition": "foggy", "humidity": 80}`,
		"Tokyo":         `{"temp": 82, "condition": "sunny", "humidity": 60}`,
	}

	if w, ok := weather[args.City]; ok {
		return w
	}
	return fmt.Sprintf(`{"error": "unknown city: %s"}`, args.City)
}
