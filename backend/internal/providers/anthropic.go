package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

// AnthropicVersion is the API version header the Messages API requires.
const AnthropicVersion = "2023-06-01"

// defaultAnthropicMaxTokens is used when neither the request nor the model
// list gives an output cap; the Messages API requires max_tokens.
const defaultAnthropicMaxTokens = 8192

// Anthropic speaks the Claude Messages API.
type Anthropic struct{ c *Clients }

func (a *Anthropic) headers(req *http.Request, key string) {
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", AnthropicVersion)
}

func (a *Anthropic) ListModels(ctx context.Context, c Credential) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out []Model
	after := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		a.headers(req, c.Key)
		resp, err := c.client(a.c).Do(req)
		if err != nil {
			return nil, &Error{Provider: c.Spec.ID, Message: "сервис недоступен: " + scrub(err.Error(), c.Key)}
		}
		if resp.StatusCode != http.StatusOK {
			err := readError(c.Spec.ID, resp)
			resp.Body.Close()
			return nil, err
		}
		var body struct {
			Data []struct {
				ID             string `json:"id"`
				DisplayName    string `json:"display_name"`
				MaxInputTokens *int   `json:"max_input_tokens"`
				MaxTokens      *int   `json:"max_tokens"`
				Capabilities   *struct {
					ImageInput *struct {
						Supported bool `json:"supported"`
					} `json:"image_input"`
				} `json:"capabilities"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, &Error{Provider: c.Spec.ID, Message: "не удалось разобрать список моделей"}
		}
		for _, m := range body.Data {
			mm := Model{Provider: c.Spec.ID, ID: m.ID, DisplayName: m.DisplayName}
			if mm.DisplayName == "" {
				mm.DisplayName = m.ID
			}
			if m.MaxInputTokens != nil && *m.MaxInputTokens > 0 {
				mm.Context = intPtr(*m.MaxInputTokens)
			}
			if m.MaxTokens != nil {
				mm.MaxOutput = *m.MaxTokens
			}
			if m.Capabilities != nil && m.Capabilities.ImageInput != nil {
				mm.Vision = boolPtr(m.Capabilities.ImageInput.Supported)
			}
			out = append(out, mm)
		}
		if !body.HasMore || body.LastID == "" {
			break
		}
		after = body.LastID
	}
	return out, nil
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

// anthropicBlocks is a message as content blocks: images first (as the
// Messages API docs recommend), then the text.
func anthropicBlocks(m Message) []map[string]any {
	blocks := make([]map[string]any, 0, len(m.Attachments)+1)
	for _, a := range m.Attachments {
		blocks = append(blocks, map[string]any{"type": "image",
			"source": map[string]string{"type": "base64", "media_type": a.MediaType, "data": a.Data}})
	}
	if m.Content != "" || len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
	}
	return blocks
}

// anthropicMessages merges consecutive messages of the same role (the API
// wants user and assistant to alternate) and drops a leading assistant turn.
func anthropicMessages(in []Message) []anthropicMessage {
	var out []anthropicMessage
	for _, m := range in {
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		if len(out) == 0 && role == "assistant" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, anthropicBlocks(m)...)
			continue
		}
		out = append(out, anthropicMessage{Role: role, Content: anthropicBlocks(m)})
	}
	return out
}

func (a *Anthropic) StreamChat(ctx context.Context, c Credential, r ChatRequest, onDelta func(string) error) (Usage, error) {
	var usage Usage
	if err := checkAttachments(r.Messages); err != nil {
		return usage, err
	}
	maxTokens := r.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}
	body := map[string]any{
		"model":      r.Model,
		"max_tokens": maxTokens,
		"messages":   anthropicMessages(r.Messages),
		"stream":     true,
	}
	if r.System != "" {
		body["system"] = r.System
	}
	if r.Temperature != nil {
		body["temperature"] = *r.Temperature
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/messages", bytes.NewReader(raw))
	if err != nil {
		return usage, err
	}
	a.headers(req, c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.client(a.c).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return usage, ctx.Err()
		}
		return usage, &Error{Provider: c.Spec.ID, Message: "сервис недоступен: " + scrub(err.Error(), c.Key)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return usage, readError(c.Spec.ID, resp)
	}
	err = ReadSSE(resp.Body, func(event, data string) error {
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Delta *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Usage *struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens int64  `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		if ev.Type == "" {
			ev.Type = event
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				usage.InputTokens = ev.Message.Usage.InputTokens
				usage.OutputTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_delta":
			if ev.Delta != nil && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				return onDelta(ev.Delta.Text)
			}
		case "message_delta":
			if ev.Usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokens != nil && *ev.Usage.InputTokens > 0 {
					usage.InputTokens = *ev.Usage.InputTokens
				}
			}
		case "message_stop":
			return io.EOF
		case "error":
			msg := "ошибка в потоке ответа"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			return &Error{Provider: c.Spec.ID, Message: msg}
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return usage, ctx.Err()
	}
	return usage, err
}
