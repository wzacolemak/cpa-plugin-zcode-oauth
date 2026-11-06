package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeSystem(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var body struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return body.System
}

func TestOfficialPromptConstants(t *testing.T) {
	if officialAgentHeader != "You are ZCode, an interactive agent" {
		t.Fatalf("unexpected header block: %q", officialAgentHeader)
	}
	// 风控要求官方正文 >= 约 1230 字符; 这里给足余量并固定当前取样长度
	if len(officialAgentBody) < 2500 {
		t.Fatalf("official body too short (%d), risk control requires the full official prompt", len(officialAgentBody))
	}
	if !strings.Contains(officialAgentBody, "# Harness") {
		t.Fatal("official body missing '# Harness' section")
	}
	if strings.Contains(officialAgentBody, "11733") {
		t.Fatal("official body still contains a machine-specific path")
	}
}

func TestInjectOfficialSystemWithoutSystem(t *testing.T) {
	in := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	out := injectOfficialSystem(in)
	blocks := decodeSystem(t, out)
	if len(blocks) != 2 {
		t.Fatalf("want 2 system blocks, got %d", len(blocks))
	}
	if blocks[0]["text"] != officialAgentHeader {
		t.Fatalf("first block is not the official header: %v", blocks[0]["text"])
	}
	if blocks[1]["text"] != officialAgentBody {
		t.Fatal("second block is not the official body")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["messages"]; !ok {
		t.Fatal("messages field was dropped")
	}
}

func TestInjectOfficialSystemWithStringSystem(t *testing.T) {
	in := []byte(`{"model":"x","system":"你是代码助手","messages":[]}`)
	blocks := decodeSystem(t, injectOfficialSystem(in))
	if len(blocks) != 3 {
		t.Fatalf("want 3 blocks, got %d", len(blocks))
	}
	if blocks[2]["text"] != "你是代码助手" {
		t.Fatalf("caller system prompt lost: %v", blocks[2])
	}
}

func TestInjectOfficialSystemWithArraySystem(t *testing.T) {
	in := []byte(`{"model":"x","system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"messages":[]}`)
	blocks := decodeSystem(t, injectOfficialSystem(in))
	if len(blocks) != 4 {
		t.Fatalf("want 4 blocks, got %d", len(blocks))
	}
	if blocks[0]["text"] != officialAgentHeader || blocks[1]["text"] != officialAgentBody {
		t.Fatal("official blocks must come first")
	}
	if blocks[2]["text"] != "A" || blocks[3]["text"] != "B" {
		t.Fatal("caller blocks must be preserved in order")
	}
}

func TestInjectOfficialSystemIsIdempotent(t *testing.T) {
	once := injectOfficialSystem([]byte(`{"model":"x","system":"hi","messages":[]}`))
	twice := injectOfficialSystem(once)
	blocks := decodeSystem(t, twice)
	if len(blocks) != 3 {
		t.Fatalf("second injection must be a no-op, got %d blocks", len(blocks))
	}
}

func TestInjectOfficialSystemKeepsOtherFields(t *testing.T) {
	in := []byte(`{"model":"glm-5.3-flash","max_tokens":64,"stream":true,"tools":[{"name":"t"}],"messages":[{"role":"user","content":"hi"}]}`)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(injectOfficialSystem(in), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "max_tokens", "stream", "tools", "messages", "system"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("field %q missing after injection", key)
		}
	}
	if string(body["model"]) != `"glm-5.3-flash"` {
		t.Fatalf("model changed: %s", body["model"])
	}
}
