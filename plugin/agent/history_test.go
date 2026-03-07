package agent_test

import (
	"context"
	"testing"

	"github.com/alexedtionweb/niro-stream"
	"github.com/alexedtionweb/niro-stream/plugin/agent"
)

func msg(role niro.Role, text string) niro.Message {
	return niro.Message{Role: role, Parts: []niro.Part{{Kind: niro.KindText, Text: text}}}
}

func TestSlidingWindow(t *testing.T) {
	policy := agent.SlidingWindow(4)
	history := []niro.Message{
		msg(niro.RoleUser, "1"),
		msg(niro.RoleAssistant, "a"),
		msg(niro.RoleUser, "2"),
		msg(niro.RoleAssistant, "b"),
		msg(niro.RoleUser, "3"),
		msg(niro.RoleAssistant, "c"),
	}
	got := policy.TrimForRequest(history)
	if len(got) != 4 {
		t.Errorf("SlidingWindow(4): got %d messages, want 4", len(got))
	}
	if got[0].Parts[0].Text != "2" || got[3].Parts[0].Text != "c" {
		t.Errorf("SlidingWindow(4): want last 4 messages (2,a,2,b,3,c -> 2,b,3,c), got %q...%q", got[0].Parts[0].Text, got[3].Parts[0].Text)
	}
}

func TestSlidingWindowNoTrim(t *testing.T) {
	policy := agent.SlidingWindow(10)
	history := []niro.Message{msg(niro.RoleUser, "hi")}
	got := policy.TrimForRequest(history)
	if len(got) != 1 {
		t.Errorf("got %d, want 1", len(got))
	}
}

func TestSlidingWindowZero(t *testing.T) {
	policy := agent.SlidingWindow(0)
	history := []niro.Message{msg(niro.RoleUser, "a"), msg(niro.RoleAssistant, "b")}
	got := policy.TrimForRequest(history)
	if len(got) != 2 {
		t.Errorf("SlidingWindow(0) should not trim; got %d", len(got))
	}
}

func TestNoHistory(t *testing.T) {
	policy := agent.NoHistory()
	history := []niro.Message{msg(niro.RoleUser, "a"), msg(niro.RoleAssistant, "b")}
	got := policy.TrimForRequest(history)
	if len(got) != 0 {
		t.Errorf("NoHistory: got %d, want 0", len(got))
	}
}

func TestKeepFirstAndLast(t *testing.T) {
	policy := agent.KeepFirstAndLast(2, 2)
	history := []niro.Message{
		msg(niro.RoleUser, "1"),
		msg(niro.RoleAssistant, "a"),
		msg(niro.RoleUser, "2"),
		msg(niro.RoleAssistant, "b"),
		msg(niro.RoleUser, "3"),
		msg(niro.RoleAssistant, "c"),
	}
	got := policy.TrimForRequest(history)
	if len(got) != 4 {
		t.Errorf("KeepFirstAndLast(2,2): got %d, want 4", len(got))
	}
	if got[0].Parts[0].Text != "1" || got[1].Parts[0].Text != "a" {
		t.Errorf("want first two 1,a; got %q,%q", got[0].Parts[0].Text, got[1].Parts[0].Text)
	}
	if got[2].Parts[0].Text != "3" || got[3].Parts[0].Text != "c" {
		t.Errorf("want last two 3,c; got %q,%q", got[2].Parts[0].Text, got[3].Parts[0].Text)
	}
}

func TestSlidingWindowTokens(t *testing.T) {
	counter := func(msgs []niro.Message) int {
		n := 0
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Kind == niro.KindText {
					n += len(p.Text) / 4
				}
			}
		}
		return n
	}
	policy := agent.SlidingWindowTokens(10, counter)
	history := []niro.Message{
		msg(niro.RoleUser, "hello world"),
		msg(niro.RoleAssistant, "hi there"),
		msg(niro.RoleUser, "last user msg"),
		msg(niro.RoleAssistant, "last asst"),
	}
	got := policy.TrimForRequest(history)
	if len(got) == 0 {
		t.Error("SlidingWindowTokens: expected at least one message")
	}
}

func TestRuntimeAppliesHistoryPolicy(t *testing.T) {
	var lastReq *niro.Request
	mem := &mockMemory{history: []niro.Message{
		msg(niro.RoleUser, "old1"),
		msg(niro.RoleAssistant, "r1"),
		msg(niro.RoleUser, "old2"),
		msg(niro.RoleAssistant, "r2"),
	}}
	rt, err := agent.New(
		niro.ProviderFunc(func(ctx context.Context, req *niro.Request) (*niro.Stream, error) {
			lastReq = req
			return niro.StreamFromSlice([]niro.Frame{niro.TextFrame("ok")}), nil
		}),
		agent.WithMemory(mem),
		agent.WithHistoryPolicy(agent.SlidingWindow(2)),
	)
	assertNoError(t, err)
	_, err = rt.Run(context.Background(), "s1", "new")
	assertNoError(t, err)
	if lastReq == nil {
		t.Fatal("provider was not called")
	}
	if len(lastReq.Messages) != 3 {
		t.Errorf("with SlidingWindow(2) expected 3 messages (2 history + new), got %d", len(lastReq.Messages))
	}
	// SlidingWindow implements HistorySavePolicy: saved history is trimmed to last 2 (new user + assistant reply)
	saved, _ := mem.Load(context.Background(), "s1")
	if len(saved) != 2 {
		t.Errorf("SlidingWindow(2) TrimForSave: expected 2 saved messages (last 2), got %d", len(saved))
	}
}

func TestChain(t *testing.T) {
	// Keep first 1, then sliding window 3 → result is min(3, 1 + tail) so we get first 1 + last 2 when we have 5 msgs
	policy := agent.Chain(agent.KeepFirstAndLast(1, 2))
	history := []niro.Message{
		msg(niro.RoleUser, "1"),
		msg(niro.RoleAssistant, "a"),
		msg(niro.RoleUser, "2"),
		msg(niro.RoleAssistant, "b"),
		msg(niro.RoleUser, "3"),
	}
	got := policy.TrimForRequest(history)
	if len(got) != 3 {
		t.Errorf("Chain(KeepFirstAndLast(1,2)): got %d messages, want 3", len(got))
	}
	if got[0].Parts[0].Text != "1" || got[2].Parts[0].Text != "3" {
		t.Errorf("want first=1, last=3; got %q, %q", got[0].Parts[0].Text, got[2].Parts[0].Text)
	}
}
