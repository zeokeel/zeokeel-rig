package events_test

// 工具结果 payload 的**无损**契约（E6）：rig.ToolResult.Data（宿主的结构化结果）
// 必须能原样穿过 NDJSON 往返，回放/UI 不必再从人读文本里反解析状态。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/zeokeel/zeokeel-rig/events"
)

// TestToolResultPayloadCarriesData：写入再读取之后，Data 的字节与写入前等价。
func TestToolResultPayloadCarriesData(t *testing.T) {
	data := json.RawMessage(`{"exit":1}`)
	payload := events.ToolResult{
		ID:      "c1",
		Name:    "bash",
		Content: "exit status 1",
		IsError: true,
		Data:    data,
	}
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	if err := emitter.Emit(events.KindToolResult, payload); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	decoder := events.NewDecoder(bytes.NewReader(buffer.Bytes()))
	envelope, err := decoder.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if envelope.Kind != events.KindToolResult {
		t.Fatalf("kind = %q", envelope.Kind)
	}
	var got events.ToolResult
	if err := json.Unmarshal(envelope.Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v（%s）", err, envelope.Payload)
	}
	if !bytes.Equal(got.Data, data) {
		t.Fatalf("Data 往返后变了：got %s, want %s", got.Data, data)
	}
	// 其余字段照旧。
	if got.ID != "c1" || got.Name != "bash" || !got.IsError || got.Content != "exit status 1" {
		t.Fatalf("payload 其他字段被改动: %+v", got)
	}
	// 无 Data 时省略字段（线格式兼容：旧消费方看不到 data）。
	empty := events.ToolResult{ID: "c2", Name: "read_file"}
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	if _, exists := fields["data"]; exists {
		t.Fatalf("Data 为 nil 时不该写出 data 字段: %s", encoded)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("流应当只有一条事件: %v", err)
	}
}
