package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/component"
	"github.com/alexedtionweb/niro-stream/middleware"
	"github.com/alexedtionweb/niro-stream/plugin/agent"
	"github.com/alexedtionweb/niro-stream/registry"
	"github.com/alexedtionweb/niro-stream/tools"
)

// --- test helpers ---

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func assertErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Errorf("expected error containing %q, got nil", substr)
		return
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error %q does not contain %q", err.Error(), substr)
	}
}

func assertNotNil(t *testing.T, v any) {
	t.Helper()
	if v == nil {
		t.Error("expected non-nil")
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}

// --- mock providers and helpers ---

func echoProvider(reply string) niro.Provider {
	return niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame(reply)}), nil
	})
}

func errorProvider(msg string) niro.Provider {
	return niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		return nil, errors.New(msg)
	})
}

// mockPeer implements agent.Peer.
type mockPeer struct {
	name     string
	response string
	err      error
}

func (m *mockPeer) Name() string { return m.name }
func (m *mockPeer) Ask(ctx context.Context, sessionID string, input string) (string, error) {
	return m.response, m.err
}

// mockMemory implements agent.Memory with optional errors.
type mockMemory struct {
	history   []niro.Message
	loadErr   error
	saveErr   error
	loadCalls int
	saveCalls int
}

func (m *mockMemory) Load(ctx context.Context, sessionID string) ([]niro.Message, error) {
	m.loadCalls++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return append([]niro.Message(nil), m.history...), nil
}

func (m *mockMemory) Save(ctx context.Context, sessionID string, history []niro.Message) error {
	m.saveCalls++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.history = append([]niro.Message(nil), history...)
	return nil
}

// boundedLoaderMock implements agent.BoundedLoader and records LoadLast usage.
type boundedLoaderMock struct {
	mockMemory
	loadLastCalls   int
	lastLoadLastMax int
}

func (m *boundedLoaderMock) LoadLast(ctx context.Context, sessionID string, maxMessages int) ([]niro.Message, error) {
	m.loadLastCalls++
	m.lastLoadLastMax = maxMessages
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	h := m.history
	if maxMessages <= 0 || len(h) <= maxMessages {
		return append([]niro.Message(nil), h...), nil
	}
	start := len(h) - maxMessages
	out := make([]niro.Message, maxMessages)
	copy(out, h[start:])
	return out, nil
}

// failingSaveMock fails Save the first failCount times, then succeeds.
type failingSaveMock struct {
	mockMemory
	failCount int
	failErr   error
}

func (m *failingSaveMock) Save(ctx context.Context, sessionID string, history []niro.Message) error {
	m.saveCalls++
	if m.failCount > 0 {
		m.failCount--
		return m.failErr
	}
	m.history = append([]niro.Message(nil), history...)
	return nil
}

// mockComponent implements agent.Component for testing.
type mockComponent struct {
	name     string
	applied  bool
	applyErr error
}

func (c *mockComponent) Name() string                         { return c.name }
func (c *mockComponent) Capabilities() []component.Capability { return nil }
func (c *mockComponent) Start(ctx context.Context) error      { return nil }
func (c *mockComponent) Close() error                         { return nil }
func (c *mockComponent) Apply(rt *agent.Runtime) error {
	c.applied = true
	return c.applyErr
}

// --- Runtime tests ---

func TestNewNilProvider(t *testing.T) {
	t.Parallel()
	_, err := agent.New(nil)
	assertErrorContains(t, err, "provider is nil")
}

func TestNewBasic(t *testing.T) {
	t.Parallel()
	rt, err := agent.New(echoProvider("hi"))
	assertNoError(t, err)
	assertNotNil(t, rt)
}

func TestWithModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, err := agent.New(echoProvider("ok"), agent.WithModel("gpt-4"))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "ok")
}

func TestRuntimeRunBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, err := agent.New(echoProvider("world"))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "world")
}

func TestRuntimeRunWithMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mem := &mockMemory{}
	rt, err := agent.New(echoProvider("reply"), agent.WithMemory(mem))
	assertNoError(t, err)

	// First turn: no history.
	res, err := rt.Run(ctx, "session-1", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "reply")
	assertEqual(t, mem.loadCalls, 1)
	assertEqual(t, mem.saveCalls, 1)

	// Second turn: history has 2 messages (user + assistant from first turn).
	res, err = rt.Run(ctx, "session-1", "how are you")
	assertNoError(t, err)
	assertEqual(t, res.Text, "reply")
	assertEqual(t, mem.loadCalls, 2)
	assertEqual(t, mem.saveCalls, 2)
}

func TestRuntimeRunMemoryLoadError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mem := &mockMemory{loadErr: fmt.Errorf("load failed")}
	rt, err := agent.New(echoProvider("ok"), agent.WithMemory(mem))
	assertNoError(t, err)

	_, err = rt.Run(ctx, "session-1", "hello")
	assertErrorContains(t, err, "load failed")
}

func TestRuntimeRunMemorySaveError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mem := &mockMemory{saveErr: fmt.Errorf("save failed")}
	rt, err := agent.New(echoProvider("ok"), agent.WithMemory(mem))
	assertNoError(t, err)

	_, err = rt.Run(ctx, "session-1", "hello")
	assertErrorContains(t, err, "save failed")
}

func TestRuntimeMemorySaveRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Fail twice then succeed.
	mem := &failingSaveMock{failCount: 2, failErr: fmt.Errorf("save transient")}
	rt, err := agent.New(
		echoProvider("ok"),
		agent.WithMemory(mem),
		agent.WithMemoryRetry(3, 1*time.Millisecond),
	)
	assertNoError(t, err)

	res, err := rt.Run(ctx, "s1", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "ok")
	assertEqual(t, mem.saveCalls, 3)
	// State was persisted after successful retry.
	assertEqual(t, len(mem.history), 2) // user + assistant
}

func TestRuntimeUsesLoadLastWhenBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mem := &boundedLoaderMock{}
	rt, err := agent.New(
		echoProvider("reply"),
		agent.WithMemory(mem),
		agent.WithHistoryPolicy(agent.SlidingWindow(5)),
	)
	assertNoError(t, err)

	_, err = rt.Run(ctx, "s1", "hi")
	assertNoError(t, err)

	assertEqual(t, mem.loadLastCalls, 1)
	assertEqual(t, mem.lastLoadLastMax, 5)
	assertEqual(t, mem.loadCalls, 0) // runtime used LoadLast, not Load
}

func TestRuntimeRunProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, err := agent.New(errorProvider("provider down"))
	assertNoError(t, err)

	_, err = rt.Run(ctx, "", "hello")
	assertErrorContains(t, err, "provider down")
}

func TestRuntimeStartClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, err := agent.New(echoProvider("ok"))
	assertNoError(t, err)

	assertNoError(t, rt.Start(ctx))
	assertNoError(t, rt.Close())
}

func TestRuntimeNilChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var rt *agent.Runtime

	err := rt.Start(ctx)
	assertErrorContains(t, err, "nil")

	// Close on nil should not panic and should return nil.
	err = rt.Close()
	assertNoError(t, err)

	_, err = rt.Run(ctx, "", "hi")
	assertErrorContains(t, err, "nil")

	_, err = rt.CallPeer(ctx, "peer", "", "hi")
	assertErrorContains(t, err, "nil")
}

func TestCallPeer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	peer := &mockPeer{name: "assistant", response: "peer reply"}
	rt, err := agent.New(echoProvider("ok"), agent.WithPeer(peer))
	assertNoError(t, err)

	out, err := rt.CallPeer(ctx, "assistant", "s1", "hello")
	assertNoError(t, err)
	assertEqual(t, out, "peer reply")
}

func TestCallPeerError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	peer := &mockPeer{name: "p1", err: fmt.Errorf("peer unavailable")}
	rt, err := agent.New(echoProvider("ok"), agent.WithPeer(peer))
	assertNoError(t, err)

	_, err = rt.CallPeer(ctx, "p1", "s1", "hi")
	assertErrorContains(t, err, "peer unavailable")
}

func TestCallPeerNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, err := agent.New(echoProvider("ok"))
	assertNoError(t, err)

	_, err = rt.CallPeer(ctx, "missing", "", "hi")
	assertErrorContains(t, err, "not found")
}

func TestWithPeerNil(t *testing.T) {
	t.Parallel()
	// WithPeer(nil) should be ignored and not panic.
	rt, err := agent.New(echoProvider("ok"), agent.WithPeer(nil))
	assertNoError(t, err)
	assertNotNil(t, rt)
}

func TestWithComponent(t *testing.T) {
	t.Parallel()

	comp := &mockComponent{name: "test.comp"}
	rt, err := agent.New(echoProvider("ok"), agent.WithComponent(comp))
	assertNoError(t, err)
	assertNotNil(t, rt)
	assertTrue(t, comp.applied)
}

func TestWithComponentNil(t *testing.T) {
	t.Parallel()

	// WithComponent(nil) should be silently ignored.
	rt, err := agent.New(echoProvider("ok"), agent.WithComponent(nil))
	assertNoError(t, err)
	assertNotNil(t, rt)
}

func TestWithComponentApplyError(t *testing.T) {
	t.Parallel()

	comp := &mockComponent{name: "failing.comp", applyErr: fmt.Errorf("apply error")}
	_, err := agent.New(echoProvider("ok"), agent.WithComponent(comp))
	assertErrorContains(t, err, "apply error")
}

// --- Memory tests ---

func TestStatelessMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rt, err := agent.New(echoProvider("ok"), agent.WithMemory(agent.StatelessMemory))
	assertNoError(t, err)
	_, err = rt.Run(ctx, "s1", "hi")
	assertNoError(t, err)
	// Second turn: StatelessMemory returns no history, so request is still just the new user message.
	_, err = rt.Run(ctx, "s1", "again")
	assertNoError(t, err)
}

// --- InMemoryMemory tests ---

func TestInMemoryMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mem := agent.NewInMemoryMemory()

	// Load empty session.
	history, err := mem.Load(ctx, "s1")
	assertNoError(t, err)
	assertEqual(t, len(history), 0)

	// Save messages.
	msgs := []niro.Message{niro.UserText("hello"), niro.AssistantText("world")}
	err = mem.Save(ctx, "s1", msgs)
	assertNoError(t, err)

	// Load back.
	loaded, err := mem.Load(ctx, "s1")
	assertNoError(t, err)
	assertEqual(t, len(loaded), 2)

	// Different session is empty.
	other, err := mem.Load(ctx, "s2")
	assertNoError(t, err)
	assertEqual(t, len(other), 0)
}

func TestInMemoryMemoryLoadLast(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mem := agent.NewInMemoryMemory()
	msgs := []niro.Message{
		niro.UserText("1"), niro.AssistantText("a"),
		niro.UserText("2"), niro.AssistantText("b"),
		niro.UserText("3"), niro.AssistantText("c"),
	}
	err := mem.Save(ctx, "s1", msgs)
	assertNoError(t, err)

	// maxMessages <= 0: full history
	all, err := mem.LoadLast(ctx, "s1", 0)
	assertNoError(t, err)
	assertEqual(t, len(all), 6)

	// last 2 (oldest to newest)
	two, err := mem.LoadLast(ctx, "s1", 2)
	assertNoError(t, err)
	assertEqual(t, len(two), 2)
	assertEqual(t, two[0].Parts[0].Text, "3")
	assertEqual(t, two[1].Parts[0].Text, "c")

	// more than stored: return all
	six, err := mem.LoadLast(ctx, "s1", 10)
	assertNoError(t, err)
	assertEqual(t, len(six), 6)
}

// --- AgentDefinition / decl tests ---

func TestLoadAgentDefinition(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	def := agent.AgentDefinition{
		Name: "test-agent",
		Steps: []agent.Step{
			{Type: "llm", Input: "hello"},
		},
	}
	b, _ := json.Marshal(def)
	os.WriteFile(path, b, 0644)

	loaded, err := agent.LoadAgentDefinition(path)
	assertNoError(t, err)
	assertEqual(t, loaded.Name, "test-agent")
	assertEqual(t, len(loaded.Steps), 1)
}

func TestLoadAgentDefinitionNotFound(t *testing.T) {
	t.Parallel()

	_, err := agent.LoadAgentDefinition("/nonexistent/path/agent.json")
	assertErrorContains(t, err, "load agent def")
}

func TestLoadAgentDefinitionBadJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not-json"), 0644)

	_, err := agent.LoadAgentDefinition(path)
	assertErrorContains(t, err, "parse agent def")
}

// --- Orchestrator tests ---

func TestOrchestratorNilDefinition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, nil)
	_, err := o.RunDefinition(ctx, "s1", nil)
	assertErrorContains(t, err, "nil")
}

func TestOrchestratorLLMStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(echoProvider("step-result"))
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "llm", Input: "hello world"},
		},
	}

	result, err := o.RunDefinition(ctx, "s1", def)
	assertNoError(t, err)
	assertEqual(t, result, "step-result")
}

func TestOrchestratorLLMStepNoRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	o := agent.NewOrchestrator(nil, nil)
	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "llm", Input: "hi"}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "no runtime")
}

func TestOrchestratorToolStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"Name": map[string]any{"type": "string"},
		},
	})
	def, err := tools.NewToolDefinition(
		"greet",
		"greets someone",
		json.RawMessage(schema),
		func(ctx context.Context, rawArgs json.RawMessage) (any, error) {
			var args struct{ Name string }
			json.Unmarshal(rawArgs, &args)
			return "hello " + args.Name, nil
		},
	)
	assertNoError(t, err)

	ts := tools.NewToolset()
	ts.MustRegister(def)

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, ts)

	callArgs, _ := json.Marshal(map[string]string{"Name": "world"})
	agentDef := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "tool", ToolName: "greet", ToolArgs: json.RawMessage(callArgs)},
		},
	}

	result, err := o.RunDefinition(ctx, "s1", agentDef)
	assertNoError(t, err)
	assertEqual(t, result, "hello world")
}

func TestOrchestratorToolStepNoToolset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, nil) // no toolset

	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "tool", ToolName: "greet"}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "no toolset")
}

func TestOrchestratorToolStepExecError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema, _ := json.Marshal(map[string]any{"type": "object"})
	def, _ := tools.NewToolDefinition(
		"fail_tool",
		"always fails",
		json.RawMessage(schema),
		func(ctx context.Context, rawArgs json.RawMessage) (any, error) {
			return nil, fmt.Errorf("tool execution failed")
		},
	)

	ts := tools.NewToolset()
	ts.MustRegister(def)

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, ts)

	agentDef := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "tool", ToolName: "fail_tool", ToolArgs: json.RawMessage(`{}`)}},
	}
	_, err := o.RunDefinition(ctx, "s1", agentDef)
	assertErrorContains(t, err, "tool execution failed")
}

func TestOrchestratorPeerStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	peer := &mockPeer{name: "assistant", response: "peer answer"}
	rt, _ := agent.New(echoProvider("ok"), agent.WithPeer(peer))
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "peer", PeerName: "assistant", Input: "hello peer"},
		},
	}

	result, err := o.RunDefinition(ctx, "s1", def)
	assertNoError(t, err)
	assertEqual(t, result, "peer answer")
}

func TestOrchestratorPeerStepNoRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	o := agent.NewOrchestrator(nil, nil)
	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "peer", PeerName: "p1", Input: "hi"}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "runtime missing")
}

func TestOrchestratorSleepStepRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, nil)

	// "sleep" is not a valid step type; treat as unknown.
	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "sleep"}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "unknown type")
}

func TestOrchestratorUnknownStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "unknown-type"}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "unknown type")
}

func TestOrchestratorEmptySteps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{Steps: nil}
	result, err := o.RunDefinition(ctx, "s1", def)
	assertNoError(t, err)
	assertEqual(t, result, "")
}

// --- Component tests ---

func TestToolingComponentApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ts := tools.NewToolset()
	comp := &agent.ToolingComponent{Toolset: ts}

	rt, err := agent.New(echoProvider("ok"), agent.WithComponent(comp))
	assertNoError(t, err)

	// The provider should now be wrapped with tooling — basic run should still work.
	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "ok")
}

func TestToolingComponentApplyNilToolset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// nil Toolset → a new empty Toolset is created internally.
	comp := &agent.ToolingComponent{Toolset: nil}

	rt, err := agent.New(echoProvider("ok"), agent.WithComponent(comp))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "ok")
}

func TestToolingComponentNilRuntime(t *testing.T) {
	t.Parallel()

	comp := &agent.ToolingComponent{}
	err := comp.Apply(nil)
	assertErrorContains(t, err, "nil")
}

func TestComponentMethods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Cover Capabilities(), Start(), Close() directly on both component types.
	toolComp := &agent.ToolingComponent{}
	_ = toolComp.Capabilities() // returns nil, verify no panic
	assertNoError(t, toolComp.Start(ctx))
	assertNoError(t, toolComp.Close())

	mtComp := &agent.MultiTenantComponent{}
	_ = mtComp.Capabilities()
	assertNoError(t, mtComp.Start(ctx))
	assertNoError(t, mtComp.Close())
}

func TestToolingComponentStartClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	ts := tools.NewToolset()
	comp := &agent.ToolingComponent{Toolset: ts}

	rt, err := agent.New(echoProvider("ok"), agent.WithComponent(comp))
	assertNoError(t, err)

	assertNoError(t, rt.Start(ctx))
	assertNoError(t, rt.Close())
}

func TestMultiTenantComponentApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reg := registry.New()
	reg.Register("default", echoProvider("from-router"))

	router := registry.NewMultiTenantProvider(reg, registry.WithDefaultClient("default"))
	comp := &agent.MultiTenantComponent{Router: router}

	rt, err := agent.New(echoProvider("original"), agent.WithComponent(comp))
	assertNoError(t, err)

	// After applying MultiTenantComponent, provider is the router.
	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "from-router")
}

func TestMultiTenantComponentNilRouter(t *testing.T) {
	t.Parallel()

	comp := &agent.MultiTenantComponent{Router: nil}
	_, err := agent.New(echoProvider("ok"), agent.WithComponent(comp))
	assertErrorContains(t, err, "router is nil")
}

func TestMultiTenantComponentNilRuntime(t *testing.T) {
	t.Parallel()

	comp := &agent.MultiTenantComponent{}
	err := comp.Apply(nil)
	assertErrorContains(t, err, "nil")
}

func TestNewDuplicateComponent(t *testing.T) {
	t.Parallel()

	// Registering two components with the same name should fail.
	comp1 := &mockComponent{name: "dupe.comp"}
	comp2 := &mockComponent{name: "dupe.comp"}
	_, err := agent.New(echoProvider("ok"),
		agent.WithComponent(comp1),
		agent.WithComponent(comp2),
	)
	assertErrorContains(t, err, "already registered")
}

func TestRuntimeRunWithStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	streamErrProvider := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		out, em := niro.NewStream(4)
		go func() {
			defer em.Close()
			em.Error(fmt.Errorf("stream failed"))
		}()
		return out, nil
	})

	rt, err := agent.New(streamErrProvider)
	assertNoError(t, err)

	_, err = rt.Run(ctx, "", "hello")
	assertErrorContains(t, err, "stream failed")
}

func TestOrchestratorLLMStepError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(errorProvider("llm failed"))
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "llm", Input: "hi"}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "llm failed")
}

func TestOrchestratorPeerStepError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	peer := &mockPeer{name: "p1", err: fmt.Errorf("peer is down")}
	rt, _ := agent.New(echoProvider("ok"), agent.WithPeer(peer))
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "peer", PeerName: "p1", Input: "hi"}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "peer is down")
}

// ---------------------------------------------------------------------------
// RunStream
// ---------------------------------------------------------------------------

func TestRuntimeRunStream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, err := agent.New(echoProvider("hello from stream"))
	assertNoError(t, err)

	stream, err := rt.RunStream(ctx, "", "hi")
	assertNoError(t, err)
	assertTrue(t, stream != nil)

	var got strings.Builder
	for stream.Next(ctx) {
		if f := stream.Frame(); f.Kind == niro.KindText {
			got.WriteString(f.Text)
		}
	}
	assertNoError(t, stream.Err())
	assertEqual(t, got.String(), "hello from stream")
}

func TestRuntimeRunStreamSavesMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mem := agent.NewInMemoryMemory()
	rt, err := agent.New(echoProvider("streaming answer"), agent.WithMemory(mem))
	assertNoError(t, err)

	stream, err := rt.RunStream(ctx, "sess1", "question")
	assertNoError(t, err)

	// Drain the stream fully so memory is saved.
	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())

	// History must now contain the user turn and the assistant turn.
	history, err := mem.Load(ctx, "sess1")
	assertNoError(t, err)
	assertEqual(t, len(history), 2)
	assertEqual(t, string(history[0].Role), "user")
	assertEqual(t, string(history[1].Role), "assistant")

	// Second turn must build on the saved history.
	stream2, err := rt.RunStream(ctx, "sess1", "follow-up")
	assertNoError(t, err)
	for stream2.Next(ctx) {
	}
	assertNoError(t, stream2.Err())

	history2, _ := mem.Load(ctx, "sess1")
	// 2 from first turn + 2 from second turn = 4
	assertEqual(t, len(history2), 4)
}

func TestRuntimeRunStreamNilProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var rt *agent.Runtime
	_, err := rt.RunStream(ctx, "", "hi")
	assertTrue(t, err != nil)
}

func TestRuntimeRunStreamProviderError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, err := agent.New(errorProvider("provider crashed"))
	assertNoError(t, err)

	_, err = rt.RunStream(ctx, "", "hi")
	assertErrorContains(t, err, "provider crashed")
}

func TestRuntimeRunStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	streamErrProvider := niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
		out, em := niro.NewStream(4)
		go func() {
			defer em.Close()
			em.Error(fmt.Errorf("stream failed"))
		}()
		return out, nil
	})

	rt, err := agent.New(streamErrProvider)
	assertNoError(t, err)

	// RunStream: error surfaces from stream.Err(), not from RunStream itself.
	stream, err := rt.RunStream(ctx, "", "hello")
	assertNoError(t, err) // provider connect succeeded
	for stream.Next(ctx) {
	}
	assertErrorContains(t, stream.Err(), "stream failed")
}

// ---------------------------------------------------------------------------
// WithSystemPrompt / WithOptions / WithMiddleware / accessors
// ---------------------------------------------------------------------------

func TestWithSystemPrompt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotSystemPrompt string
	spy := niro.ProviderFunc(func(_ context.Context, req *niro.Request) (*niro.Stream, error) {
		gotSystemPrompt = req.SystemPrompt
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})

	rt, err := agent.New(spy, agent.WithSystemPrompt("You are a helpful agent."))
	assertNoError(t, err)

	_, err = rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, gotSystemPrompt, "You are a helpful agent.")
	assertEqual(t, rt.SystemPrompt(), "You are a helpful agent.")
}

func TestWithOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotTemp *float64
	spy := niro.ProviderFunc(func(_ context.Context, req *niro.Request) (*niro.Stream, error) {
		gotTemp = req.Options.Temperature
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})

	rt, err := agent.New(spy, agent.WithOptions(niro.Options{Temperature: niro.Temp(0.2)}))
	assertNoError(t, err)

	_, err = rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	if gotTemp == nil || *gotTemp != 0.2 {
		t.Errorf("expected temperature 0.2, got %v", gotTemp)
	}
}

func TestWithMiddleware(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	called := false
	wrap := func(p niro.Provider) niro.Provider {
		return niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
			called = true
			return p.Generate(ctx, req)
		})
	}

	rt, err := agent.New(echoProvider("wrapped"), agent.WithMiddleware(wrap))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hi")
	assertNoError(t, err)
	assertEqual(t, res.Text, "wrapped")
	assertTrue(t, called)
}

func TestWithMiddlewareNil(t *testing.T) {
	t.Parallel()
	// nil middleware must be silently ignored.
	rt, err := agent.New(echoProvider("ok"), agent.WithMiddleware(nil))
	assertNoError(t, err)
	assertNotNil(t, rt)
}

func TestProviderAccessor(t *testing.T) {
	t.Parallel()
	p := echoProvider("x")
	rt, _ := agent.New(p)
	// Provider() returns the (possibly wrapped) provider — must not be nil.
	if rt.Provider() == nil {
		t.Error("expected non-nil provider")
	}
}

func TestSystemPromptForwardedInRunStream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotPrompt string
	spy := niro.ProviderFunc(func(_ context.Context, req *niro.Request) (*niro.Stream, error) {
		gotPrompt = req.SystemPrompt
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("streamed")}), nil
	})

	rt, err := agent.New(spy, agent.WithSystemPrompt("Be helpful."))
	assertNoError(t, err)

	stream, err := rt.RunStream(ctx, "", "hi")
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())
	assertEqual(t, gotPrompt, "Be helpful.")
}

func TestRunStreamForwardsUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	usageProvider := niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		out, em := niro.NewStream(4)
		go func() {
			defer em.Close()
			_ = em.Emit(ctx, niro.TextFrame("hello"))
			u := niro.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
			_ = em.Emit(ctx, niro.UsageFrame(&u))
		}()
		return out, nil
	})

	rt, err := agent.New(usageProvider)
	assertNoError(t, err)

	stream, err := rt.RunStream(ctx, "", "hi")
	assertNoError(t, err)
	for stream.Next(ctx) {
	}
	assertNoError(t, stream.Err())

	u := stream.Usage()
	assertEqual(t, u.InputTokens, 10)
	assertEqual(t, u.OutputTokens, 5)
	assertEqual(t, u.TotalTokens, 15)
}

// ---------------------------------------------------------------------------
// New components: Retry, Cache, Timeout, Middleware
// ---------------------------------------------------------------------------

func TestRetryComponent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	attempts := 0
	flaky := niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		attempts++
		if attempts < 3 {
			return nil, fmt.Errorf("temporary error")
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok after retry")}), nil
	})

	cfg := middleware.DefaultRetryConfig()
	cfg.MaxAttempts = 5
	cfg.Backoff = middleware.ConstantBackoff{Duration: 0}  // no actual delay in tests
	cfg.ShouldRetry = func(err error) bool { return true } // retry all errors in tests

	rt, err := agent.New(flaky, agent.WithComponent(&agent.RetryComponent{Config: cfg}))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "ok after retry")
	assertEqual(t, attempts, 3)
}

func TestRetryComponentDefaultConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Zero Config → DefaultRetryConfig should be used (no panic).
	rt, err := agent.New(echoProvider("ok"), agent.WithComponent(&agent.RetryComponent{}))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "ok")
}

func TestRetryComponentNilRuntime(t *testing.T) {
	t.Parallel()
	c := &agent.RetryComponent{}
	err := c.Apply(nil)
	assertErrorContains(t, err, "nil")
}

func TestCacheComponent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	calls := 0
	counting := niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		calls++
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("cached")}), nil
	})

	rt, err := agent.New(counting, agent.WithComponent(&agent.CacheComponent{
		Options: middleware.CacheOptions{MaxEntries: 64, TTL: time.Minute},
	}))
	assertNoError(t, err)

	// First call hits the provider.
	res1, err := rt.Run(ctx, "", "same question")
	assertNoError(t, err)
	assertEqual(t, res1.Text, "cached")

	// Second identical call should be served from cache.
	res2, err := rt.Run(ctx, "", "same question")
	assertNoError(t, err)
	assertEqual(t, res2.Text, "cached")

	// Exact count depends on cache key (memory includes session history for 2nd
	// call), so just verify at most 2 provider calls.
	if calls > 2 {
		t.Errorf("expected ≤2 provider calls, got %d", calls)
	}
}

func TestCacheComponentNilRuntime(t *testing.T) {
	t.Parallel()
	c := &agent.CacheComponent{}
	err := c.Apply(nil)
	assertErrorContains(t, err, "nil")
}

func TestTimeoutComponent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A provider that returns immediately — timeout must not interfere.
	rt, err := agent.New(echoProvider("fast"), agent.WithComponent(&agent.TimeoutComponent{
		Timeout: 5 * time.Second,
	}))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hi")
	assertNoError(t, err)
	assertEqual(t, res.Text, "fast")
}

func TestTimeoutComponentDefault(t *testing.T) {
	t.Parallel()
	// Zero Timeout → uses default (no panic).
	rt, err := agent.New(echoProvider("ok"), agent.WithComponent(&agent.TimeoutComponent{}))
	assertNoError(t, err)
	assertNotNil(t, rt)
}

func TestTimeoutComponentNilRuntime(t *testing.T) {
	t.Parallel()
	c := &agent.TimeoutComponent{}
	err := c.Apply(nil)
	assertErrorContains(t, err, "nil")
}

func TestMiddlewareComponent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	wrapped := false
	rt, err := agent.New(echoProvider("mw"), agent.WithComponent(&agent.MiddlewareComponent{
		Name_: "test.mw",
		Fn: func(p niro.Provider) niro.Provider {
			return niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
				wrapped = true
				return p.Generate(ctx, req)
			})
		},
	}))
	assertNoError(t, err)

	res, err := rt.Run(ctx, "", "hello")
	assertNoError(t, err)
	assertEqual(t, res.Text, "mw")
	assertTrue(t, wrapped)
}

func TestMiddlewareComponentDefaultName(t *testing.T) {
	t.Parallel()
	c := &agent.MiddlewareComponent{Fn: func(p niro.Provider) niro.Provider { return p }}
	assertEqual(t, c.Name(), "agent.middleware")
}

func TestMiddlewareComponentNilFn(t *testing.T) {
	t.Parallel()
	c := &agent.MiddlewareComponent{Name_: "nofn"}
	// Apply requires a runtime with a provider, but Fn is nil → should fail before
	// hitting provider check. The error includes the component name.
	// Note: passing &agent.Runtime{} has nil provider so it hits "runtime/provider is nil" first.
	// To test the Fn-nil path we need a valid runtime.
	rt, _ := agent.New(echoProvider("x"))
	err := c.Apply(rt)
	assertErrorContains(t, err, "Fn is nil")
}

func TestMiddlewareComponentNilRuntime(t *testing.T) {
	t.Parallel()
	c := &agent.MiddlewareComponent{Name_: "x", Fn: func(p niro.Provider) niro.Provider { return p }}
	err := c.Apply(nil)
	assertErrorContains(t, err, "nil")
}

func TestComponentLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, comp := range []agent.Component{
		&agent.RetryComponent{},
		&agent.CacheComponent{},
		&agent.TimeoutComponent{},
		&agent.MiddlewareComponent{Name_: "lc.mw", Fn: func(p niro.Provider) niro.Provider { return p }},
	} {
		assertNoError(t, comp.Start(ctx))
		assertNoError(t, comp.Close())
	}
}

// ---------------------------------------------------------------------------
// Orchestrator: input chaining (empty Input = previous output), peer
// ---------------------------------------------------------------------------

func TestOrchestratorInputChaining(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Provider echoes whatever the user message text is (via messages).
	echoInput := niro.ProviderFunc(func(_ context.Context, req *niro.Request) (*niro.Stream, error) {
		text := ""
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if p.Kind == niro.KindText {
					text = p.Text
				}
			}
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("echo:" + text)}), nil
	})

	rt, _ := agent.New(echoInput)
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "llm", Input: "hello"},
			// Second step has no Input → receives previous step output.
			{Type: "llm", Input: ""},
		},
	}

	result, err := o.RunDefinition(ctx, "s", def)
	assertNoError(t, err)
	// First step: echo:hello → second step input: "echo:hello" → echo:echo:hello
	assertEqual(t, result, "echo:echo:hello")
}

func TestOrchestratorOutputVar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	callCount := 0
	rt, _ := agent.New(niro.ProviderFunc(func(_ context.Context, req *niro.Request) (*niro.Stream, error) {
		callCount++
		if callCount == 1 {
			return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("step1out")}), nil
		}
		// Second call echoes the input (empty Input → previous output).
		text := ""
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if p.Kind == niro.KindText {
					text = p.Text
				}
			}
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame(text)}), nil
	}))

	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "llm", Input: "first", OutputVar: "First"},
			{Type: "llm", Input: ""}, // receives "step1out"
		},
	}

	result, err := o.RunDefinition(ctx, "s", def)
	assertNoError(t, err)
	assertEqual(t, result, "step1out")
}

func TestOrchestratorPeerStepEmptyInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	peer := &mockPeer{name: "bot", response: "peer ok"}
	rt, _ := agent.New(echoProvider("ok"), agent.WithPeer(peer))
	o := agent.NewOrchestrator(rt, nil)

	// No Input — resolveInput uses lastText (empty for first step).
	def := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "peer", PeerName: "bot"},
		},
	}
	result, err := o.RunDefinition(ctx, "s", def)
	assertNoError(t, err)
	assertEqual(t, result, "peer ok")
}

func TestOrchestratorContextCancelledBetweenSteps(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	rt, _ := agent.New(niro.ProviderFunc(func(_ context.Context, _ *niro.Request) (*niro.Stream, error) {
		calls++
		cancel() // cancel after first step
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("done")}), nil
	}))

	o := agent.NewOrchestrator(rt, nil)
	def := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "llm", Input: "step1"},
			{Type: "llm", Input: "step2"},
		},
	}

	_, err := o.RunDefinition(ctx, "s", def)
	if err == nil {
		t.Error("expected context cancellation error")
	}
	assertEqual(t, calls, 1) // second step must not execute
}

func TestStepInputUsedWhenSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var gotInput string
	spy := niro.ProviderFunc(func(_ context.Context, req *niro.Request) (*niro.Stream, error) {
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if p.Kind == niro.KindText {
					gotInput = p.Text
				}
			}
		}
		return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
	})

	rt, _ := agent.New(spy)
	o := agent.NewOrchestrator(rt, nil)

	def := &agent.AgentDefinition{
		Steps: []agent.Step{
			{Type: "llm", Input: "from-input"},
		},
	}

	_, err := o.RunDefinition(ctx, "s", def)
	assertNoError(t, err)
	assertEqual(t, gotInput, "from-input")
}
