package voca

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSSETextDeltas(t *testing.T) {
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n: keepalive\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\ndata: [DONE]\n\n"
	var out bytes.Buffer
	wrote, err := parseSSE(strings.NewReader(stream), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || out.String() != "Hello world" {
		t.Fatalf("wrote=%v output=%q", wrote, out.String())
	}
}

func TestParseSSETerminalOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   string
	}{
		{name: "truncated", stream: `data: {"type":"response.output_text.delta","delta":"partial"}

`, want: "before response.completed"},
		{name: "failed", stream: `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"generation failed"}}}

`, want: "server_error"},
		{name: "incomplete", stream: `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}

`, want: "max_output_tokens"},
		{name: "error", stream: `data: {"type":"error","code":"bad_request","message":"bad input","param":null}

`, want: "bad input"},
		{name: "refusal", stream: `data: {"type":"response.refusal.delta","delta":"I cannot help"}

data: {"type":"response.completed","response":{"status":"completed"}}

`, want: "refused"},
		{name: "completed without text", stream: `data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"computer_screenshot"}]}]}}

`, want: "without text output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSSE(strings.NewReader(tt.stream), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParseSSERejectsMalformedData(t *testing.T) {
	if _, err := parseSSE(strings.NewReader("data: nope\n\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestAIClientRequestAndFinalNewline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request) != 4 || request["model"] != "gpt-5.6-luna" || request["input"] != "prompt" || request["stream"] != true {
			t.Errorf("request = %#v", request)
		}
		reasoning, ok := request["reasoning"].(map[string]any)
		if !ok || len(reasoning) != 1 || reasoning["effort"] != "low" {
			t.Errorf("reasoning = %#v", request["reasoning"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"lesson\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	}))
	defer server.Close()
	var out bytes.Buffer
	client := AIClient{HTTP: server.Client(), Endpoint: server.URL, APIKey: "secret"}
	if err := client.Generate(context.Background(), "prompt", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "lesson\n" {
		t.Fatalf("output = %q", out.String())
	}
}
