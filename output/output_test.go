package output

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alexedtionweb/niro-stream"
)

func TestRouteDispatch(t *testing.T) {
	ctx := context.Background()
	var text, thinking, toolCall, toolResult, custom []string
	sink := &Sink{
		OnText: func(ctx context.Context, s string) error {
			text = append(text, s)
			return nil
		},
		OnThinking: func(ctx context.Context, s string) error {
			thinking = append(thinking, s)
			return nil
		},
		OnToolCall: func(ctx context.Context, c *niro.ToolCall) error {
			toolCall = append(toolCall, c.Name)
			return nil
		},
		OnToolResult: func(ctx context.Context, r *niro.ToolResult) error {
			toolResult = append(toolResult, r.CallID+":"+r.Content)
			return nil
		},
		OnCustom: func(ctx context.Context, typ string, data any) error {
			custom = append(custom, typ+":"+fmt.Sprint(data))
			return nil
		},
	}
	frames := []niro.Frame{
		niro.TextFrame("hello "),
		niro.TextFrame("world"),
		niro.Frame{Kind: niro.KindCustom, Custom: &niro.ExperimentalFrame{Type: niro.CustomThinking, Data: "reasoning step"}},
		niro.ToolCallFrame(&niro.ToolCall{ID: "1", Name: "foo", Args: nil}),
		niro.Frame{Kind: niro.KindToolResult, Result: &niro.ToolResult{CallID: "1", Content: "ok"}},
		niro.Frame{Kind: niro.KindCustom, Custom: &niro.ExperimentalFrame{Type: niro.CustomHandoff, Data: "agent_x"}},
	}
	src := niro.StreamFromSlice(frames)
	routed := Route(ctx, src, sink)
	var got []niro.Frame
	for routed.Next(ctx) {
		got = append(got, routed.Frame())
	}
	if err := routed.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Errorf("got %d frames, want 6", len(got))
	}
	if len(toolResult) != 1 || toolResult[0] != "1:ok" {
		t.Errorf("OnToolResult: got %v", toolResult)
	}
	if g := strings.Join(text, ""); g != "hello world" {
		t.Errorf("OnText: got %q", g)
	}
	if len(thinking) != 1 || thinking[0] != "reasoning step" {
		t.Errorf("OnThinking: got %v", thinking)
	}
	if len(toolCall) != 1 || toolCall[0] != "foo" {
		t.Errorf("OnToolCall: got %v", toolCall)
	}
	if len(custom) != 1 || custom[0] != "handoff:agent_x" {
		t.Errorf("OnCustom: got %v", custom)
	}
}

func TestRouteNilSink(t *testing.T) {
	ctx := context.Background()
	src := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("x")})
	out := Route(ctx, src, nil)
	if out != src {
		t.Error("Route with nil sink should return original stream")
	}
}

func TestRouteAgentAttribution(t *testing.T) {
	ctx := context.Background()
	const agentName = "test-agent"

	var startAgent string
	var textAgents, endAgents []string

	sink := &Sink{
		OnAgentStart: func(ctx context.Context, agent string) error {
			startAgent = agent
			if got := AgentFromContext(ctx); got != agentName {
				t.Errorf("OnAgentStart: AgentFromContext = %q, want %q", got, agentName)
			}
			return nil
		},
		OnText: func(ctx context.Context, text string) error {
			textAgents = append(textAgents, AgentFromContext(ctx))
			return nil
		},
		OnEnd: func(ctx context.Context, u niro.Usage, resp *niro.ResponseMeta) error {
			endAgents = append(endAgents, AgentFromContext(ctx))
			return nil
		},
	}

	frames := []niro.Frame{niro.TextFrame("hello"), niro.TextFrame(" world")}
	src := niro.StreamFromSlice(frames)
	routed := RouteAgent(ctx, src, sink, agentName)

	for routed.Next(ctx) {
	}
	if err := routed.Err(); err != nil {
		t.Fatal(err)
	}

	if startAgent != agentName {
		t.Errorf("OnAgentStart agent = %q, want %q", startAgent, agentName)
	}
	if len(textAgents) != 2 || textAgents[0] != agentName || textAgents[1] != agentName {
		t.Errorf("OnText agents = %v, want [%q, %q]", textAgents, agentName, agentName)
	}
	if len(endAgents) != 1 || endAgents[0] != agentName {
		t.Errorf("OnEnd agents = %v, want [%q]", endAgents, agentName)
	}
}

func TestRouteAgentEmpty(t *testing.T) {
	ctx := context.Background()
	var started bool
	sink := &Sink{
		OnAgentStart: func(ctx context.Context, agent string) error {
			started = true
			return nil
		},
		OnText: func(ctx context.Context, text string) error {
			if got := AgentFromContext(ctx); got != "" {
				t.Errorf("AgentFromContext = %q, want empty", got)
			}
			return nil
		},
	}
	src := niro.StreamFromSlice([]niro.Frame{niro.TextFrame("x")})
	routed := RouteAgent(ctx, src, sink, "")
	for routed.Next(ctx) {
	}
	if started {
		t.Error("OnAgentStart should not be called when agent is empty")
	}
}
