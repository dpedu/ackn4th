package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dethmasque/ackn4th/internal/provider"
)

// A compatible API (DeepSeek) returns reasoning as text rather than
// encrypted: it streams to the dashboard and goes back with the tool
// results, as such APIs require once tools are offered.
func TestPlainReasoning(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-broke" {
			w.WriteHeader(402)
			io.WriteString(w, `{"error":{"message":"Insufficient Balance"}}`)
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if len(bodies) == 1 {
			sse(w,
				`{"type":"response.reasoning_text.delta","item_id":"rs_1","output_index":0,"content_index":0,"delta":"Read a.go first.","sequence_number":1}`,
				`{"type":"response.completed","sequence_number":2,"response":{"id":"r1","object":"response","status":"completed","model":"deepseek-v4-pro","output":[
					{"type":"reasoning","id":"rs_1","summary":null,"content":[{"type":"reasoning_text","text":"Read a.go first."}]},
					{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}","status":"completed"}],
					"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":90},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":107}}}`,
			)
			return
		}
		sse(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"r2","object":"response","status":"completed","model":"deepseek-v4-pro","output":[
			{"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Done.","annotations":[]}]}],
			"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":100},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":122}}}`)
	}))
	defer srv.Close()

	p := New(Config{APIKey: "sk-test", Model: "deepseek-v4-pro", Effort: "high", BaseURL: srv.URL + "/"})
	s := p.NewSession(provider.System{Shared: "team"}, []provider.ToolSpec{{Name: "read_file", Description: "read", Schema: map[string]any{"type": "object"}}})
	var thought strings.Builder
	r, err := s.SendText(context.Background(), "go", provider.Stream{Thinking: func(t string) { thought.WriteString(t) }})
	if err != nil || r.StopReason != provider.StopToolUse {
		t.Fatalf("reply = %+v, %v", r, err)
	}
	if thought.String() != "Read a.go first." {
		t.Errorf("reasoning streamed as %q", thought.String())
	}
	if _, err := s.SendToolResults(context.Background(), []provider.ToolResult{{CallID: "call_1", Content: "package a"}}, "", provider.Stream{}); err != nil {
		t.Fatal(err)
	}
	rs := bodies[1]["input"].([]any)[1].(map[string]any)
	parts, _ := rs["content"].([]any)
	if rs["type"] != "reasoning" || len(parts) != 1 || parts[0].(map[string]any)["text"] != "Read a.go first." || rs["encrypted_content"] != nil {
		t.Errorf("the reasoning should go back as text: %v", rs)
	}

	broke := New(Config{APIKey: "sk-broke", Model: "m", BaseURL: srv.URL + "/"})
	_, err = broke.NewSession(provider.System{}, nil).SendText(context.Background(), "x", provider.Stream{})
	if err == nil || !strings.Contains(err.Error(), "out of API credits") {
		t.Errorf("402: %v", err)
	}
}
