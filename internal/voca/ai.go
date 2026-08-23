package voca

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const DefaultAIEndpoint = "https://opencode.ai/zen/go/v1/responses"

type AIClient struct {
	HTTP     *http.Client
	Endpoint string
	APIKey   string
}

type responsesRequest struct {
	Model     string             `json:"model"`
	Input     string             `json:"input"`
	Reasoning responsesReasoning `json:"reasoning"`
	Stream    bool               `json:"stream"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

func (c AIClient) Generate(ctx context.Context, prompt string, out io.Writer) error {
	if c.APIKey == "" {
		return fmt.Errorf("OPENCODE_GO_API_KEY is not set")
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultAIEndpoint
	}
	body, err := json.Marshal(responsesRequest{
		Model:     "gpt-5.6-luna",
		Input:     prompt,
		Reasoning: responsesReasoning{Effort: "low"},
		Stream:    true,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("AI request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("AI request returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	tracked := &lastByteWriter{writer: out}
	wrote, err := parseSSE(resp.Body, tracked)
	if err != nil {
		return err
	}
	if wrote && tracked.last != '\n' {
		if _, err := io.WriteString(out, "\n"); err != nil {
			return err
		}
	}
	return nil
}

func parseSSE(r io.Reader, out io.Writer) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var data []string
	wrote := false
	streamedText := false
	completed := false
	refusal := ""
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		raw := strings.Join(data, "\n")
		data = data[:0]
		if raw == "[DONE]" {
			return nil
		}
		var event responseStreamEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return fmt.Errorf("decode AI stream event: %w", err)
		}
		switch event.Type {
		case "response.output_text.delta":
			if event.Delta == "" {
				return nil
			}
			if _, err := io.WriteString(out, event.Delta); err != nil {
				return err
			}
			wrote = true
			streamedText = true
		case "response.refusal.delta":
			refusal += event.Delta
		case "response.refusal.done":
			if event.Refusal != "" {
				refusal = event.Refusal
			}
		case "response.failed":
			return responseFailure(event)
		case "response.incomplete":
			return responseIncomplete(event)
		case "error":
			return responseError(event)
		case "response.completed":
			if event.Response != nil && event.Response.Status != "" && event.Response.Status != "completed" {
				return fmt.Errorf("AI response completed with status %q", event.Response.Status)
			}
			if event.Response != nil {
				for _, item := range event.Response.Output {
					for _, content := range item.Content {
						if content.Type == "refusal" && content.Refusal != "" {
							refusal = content.Refusal
						}
						if !streamedText && content.Type == "output_text" && content.Text != "" {
							if _, err := io.WriteString(out, content.Text); err != nil {
								return err
							}
							wrote = true
						}
					}
				}
			}
			completed = true
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return wrote, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return wrote, fmt.Errorf("read AI stream: %w", err)
	}
	if err := flush(); err != nil {
		return wrote, err
	}
	if !completed {
		return wrote, fmt.Errorf("AI stream ended before response.completed")
	}
	if refusal != "" {
		return wrote, fmt.Errorf("AI response was refused: %s", refusal)
	}
	if !wrote {
		return false, fmt.Errorf("AI response completed without text output")
	}
	return wrote, nil
}

type responseStreamEvent struct {
	Type     string            `json:"type"`
	Delta    string            `json:"delta"`
	Refusal  string            `json:"refusal"`
	Code     string            `json:"code"`
	Message  string            `json:"message"`
	Error    *responseAPIError `json:"error"`
	Response *struct {
		Status            string            `json:"status"`
		Error             *responseAPIError `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
	} `json:"response"`
}

type responseAPIError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   any    `json:"param"`
}

func responseFailure(event responseStreamEvent) error {
	if event.Response != nil && event.Response.Error != nil {
		return fmt.Errorf("AI response failed%s", formatAPIError(event.Response.Error.Code, event.Response.Error.Message))
	}
	return fmt.Errorf("AI response failed")
}

func responseIncomplete(event responseStreamEvent) error {
	if event.Response != nil && event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason != "" {
		return fmt.Errorf("AI response incomplete: %s", event.Response.IncompleteDetails.Reason)
	}
	return fmt.Errorf("AI response incomplete")
}

func responseError(event responseStreamEvent) error {
	if event.Message != "" || event.Code != "" {
		return fmt.Errorf("AI stream error%s", formatAPIError(event.Code, event.Message))
	}
	if event.Error != nil {
		return fmt.Errorf("AI stream error%s", formatAPIError(event.Error.Code, event.Error.Message))
	}
	return fmt.Errorf("AI stream error")
}

func formatAPIError(code, message string) string {
	if code != "" && message != "" {
		return fmt.Sprintf(" [%s]: %s", code, message)
	}
	if message != "" {
		return ": " + message
	}
	if code != "" {
		return " [" + code + "]"
	}
	return ""
}

type lastByteWriter struct {
	writer io.Writer
	last   byte
}

func (w *lastByteWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.last = p[n-1]
	}
	return n, err
}
