package compact

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func textMessage(role rig.Role, text string) rig.Message { return rig.NewMessage(role, text) }

func toolMessage(id, text string) rig.Message {
	return rig.Message{Role: rig.RoleTool, Content: []rig.ContentPart{rig.TextPart(text)}, ToolCallID: id}
}

func TestWindowCompactorKeepsSystemAndTail(t *testing.T) {
	messages := []rig.Message{
		textMessage(rig.RoleSystem, "system"),
		textMessage(rig.RoleUser, "u1"),
		textMessage(rig.RoleAssistant, "a1"),
		textMessage(rig.RoleUser, "u2"),
		textMessage(rig.RoleAssistant, "a2"),
		textMessage(rig.RoleUser, "u3"),
	}
	out, err := WindowCompactor{KeepLast: 2}.Compact(context.Background(), "s", messages, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("compacted len = %d, want 4: %#v", len(out), out)
	}
	if out[0].Role != rig.RoleSystem || out[0].Text() != "system" {
		t.Fatalf("system message not preserved: %#v", out[0])
	}
	if !IsCompactionSummary(out[1]) || !strings.Contains(out[1].Text(), "3 messages") {
		t.Fatalf("summary = %#v", out[1])
	}
	if out[3].Text() != "u3" {
		t.Fatalf("tail = %#v", out[2:])
	}
}

func TestWindowCompactorKeepsToolBlockTogether(t *testing.T) {
	messages := []rig.Message{
		textMessage(rig.RoleUser, "run"),
		{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{{ID: "1", Name: "bash", Arguments: `{}`}}},
		toolMessage("1", "output"),
		textMessage(rig.RoleAssistant, "done"),
	}
	// keepLast=1 would start the tail at the tool result; the compactor must
	// move the boundary back to include the assistant tool-call message.
	out, err := WindowCompactor{KeepLast: 2}.Compact(context.Background(), "s", messages, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("len = %d, want summary+tool block+done: %#v", len(out), out)
	}
	if len(out[1].ToolCalls) == 0 || out[2].Role != rig.RoleTool {
		t.Fatalf("tool block not retained together: %#v", out)
	}
}

func TestOffloadOldImages(t *testing.T) {
	image := rig.Message{Role: rig.RoleUser, Content: []rig.ContentPart{rig.TextPart("old"), rig.ImagePart("data:image/png;base64,AAAA", "")}}
	newImage := rig.Message{Role: rig.RoleUser, Content: []rig.ContentPart{rig.ImagePart("data:image/png;base64,BBBB", "")}}
	messages := []rig.Message{image, textMessage(rig.RoleAssistant, "a"), newImage}
	out := OffloadOldImages(messages, 1)
	if out[0].Content[1].Type != rig.PartText || out[0].Content[1].Text != ImageOffloadPlaceholder {
		t.Fatalf("old image not offloaded: %#v", out[0])
	}
	if out[2].Content[0].Type != rig.PartImageURL {
		t.Fatalf("new image changed: %#v", out[2])
	}
	if messages[0].Content[1].Type != rig.PartImageURL {
		t.Fatal("input messages mutated")
	}
}

func TestPruneToolResults(t *testing.T) {
	messages := []rig.Message{toolMessage("1", "old"), toolMessage("2", "new")}
	out := PruneToolResults(messages, 1)
	if out[0].Text() != ToolResultPrunedMarker || out[1].Text() != "new" {
		t.Fatalf("prune = %#v", out)
	}
}

type fakeStream struct {
	chunks []rig.Chunk
	err    error
	index  int
}

func (s *fakeStream) Recv() (rig.Chunk, error) {
	if s.index >= len(s.chunks) {
		if s.err != nil {
			return rig.Chunk{}, s.err
		}
		return rig.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}

type fakeProvider struct {
	stream   rig.Stream
	err      error
	requests []rig.Request
}

func (p *fakeProvider) Name() string { return "fake" }
func (p *fakeProvider) Stream(_ context.Context, req rig.Request) (rig.Stream, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return nil, p.err
	}
	return p.stream, nil
}

func TestLLMCompactorSummarizes(t *testing.T) {
	provider := &fakeProvider{stream: &fakeStream{chunks: []rig.Chunk{{Delta: "summary text"}}}}
	messages := []rig.Message{
		textMessage(rig.RoleUser, "u1"), textMessage(rig.RoleAssistant, "a1"),
		textMessage(rig.RoleUser, "u2"), textMessage(rig.RoleAssistant, "a2"),
		textMessage(rig.RoleUser, "u3"),
	}
	out, err := LLMCompactor{Provider: provider, KeepLast: 1}.Compact(context.Background(), "s", messages, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !IsCompactionSummary(out[0]) || !strings.Contains(out[0].Text(), "summary text") {
		t.Fatalf("summary = %#v", out[0])
	}
	if len(provider.requests) != 1 || !strings.Contains(provider.requests[0].Messages[0].Text(), "summarizer") {
		t.Fatalf("summary request = %#v", provider.requests)
	}
}

func TestLLMCompactorFallsBackOnError(t *testing.T) {
	provider := &fakeProvider{err: errors.New("boom")}
	messages := []rig.Message{
		textMessage(rig.RoleUser, "u1"), textMessage(rig.RoleAssistant, "a1"),
		textMessage(rig.RoleUser, "u2"), textMessage(rig.RoleAssistant, "a2"),
		textMessage(rig.RoleUser, "u3"),
	}
	out, err := LLMCompactor{Provider: provider, KeepLast: 1}.Compact(context.Background(), "s", messages, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !IsCompactionSummary(out[0]) || strings.Contains(out[0].Text(), "summary text") {
		t.Fatalf("fallback output = %#v", out)
	}
}

func TestLLMCompactorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LLMCompactor{Provider: &fakeProvider{}}.Compact(ctx, "s", []rig.Message{textMessage(rig.RoleUser, "x")}, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
