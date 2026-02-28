package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ryn.dev/ryn"
	"ryn.dev/ryn/component"
	"ryn.dev/ryn/plugin/agent"
	"ryn.dev/ryn/registry"
	"ryn.dev/ryn/tools"
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

func echoProvider(reply string) ryn.Provider {
	return ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return ryn.StreamFromSlice([]ryn.Frame{ryn.TextFrame(reply)}), nil
	})
}

func errorProvider(msg string) ryn.Provider {
	return ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		return nil, fmt.Errorf(msg)
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
	history   []ryn.Message
	loadErr   error
	saveErr   error
	loadCalls int
	saveCalls int
}

func (m *mockMemory) Load(ctx context.Context, sessionID string) ([]ryn.Message, error) {
	m.loadCalls++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return append([]ryn.Message(nil), m.history...), nil
}

func (m *mockMemory) Save(ctx context.Context, sessionID string, history []ryn.Message) error {
	m.saveCalls++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.history = append([]ryn.Message(nil), history...)
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
	msgs := []ryn.Message{ryn.UserText("hello"), ryn.AssistantText("world")}
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

// --- AgentDefinition / decl tests ---

func TestLoadAgentDefinition(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	def := agent.AgentDefinition{
		Name: "test-agent",
		Steps: []agent.Step{
			{Type: "llm", Messages: []string{"hello"}},
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
			{Type: "llm", Messages: []string{"hello world"}},
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
		Steps: []agent.Step{{Type: "llm", Messages: []string{"hi"}}},
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
		"fail-tool",
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
		Steps: []agent.Step{{Type: "tool", ToolName: "fail-tool", ToolArgs: json.RawMessage(`{}`)}},
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
			{Type: "peer", PeerName: "assistant", Messages: []string{"hello peer"}},
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
		Steps: []agent.Step{{Type: "peer", PeerName: "p1", Messages: []string{"hi"}}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "runtime missing")
}

func TestOrchestratorSleepStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rt, _ := agent.New(echoProvider("ok"))
	o := agent.NewOrchestrator(rt, nil)

	// SleepSeconds=0 → no actual sleep.
	def := &agent.AgentDefinition{
		Steps: []agent.Step{{Type: "sleep", SleepSeconds: 0}},
	}
	result, err := o.RunDefinition(ctx, "s1", def)
	assertNoError(t, err)
	assertEqual(t, result, "") // no output from sleep step
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

func TestRuntimeRunStreamError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	streamErrProvider := ryn.ProviderFunc(func(ctx context.Context, req *ryn.Request) (*ryn.Stream, error) {
		out, em := ryn.NewStream(4)
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
		Steps: []agent.Step{{Type: "llm", Messages: []string{"hi"}}},
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
		Steps: []agent.Step{{Type: "peer", PeerName: "p1", Messages: []string{"hi"}}},
	}
	_, err := o.RunDefinition(ctx, "s1", def)
	assertErrorContains(t, err, "peer is down")
}
