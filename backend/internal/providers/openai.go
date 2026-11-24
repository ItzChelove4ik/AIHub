package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// OpenAI speaks the Chat Completions API: OpenAI itself, OpenRouter, xAI,
// DeepSeek, Mistral, Groq and any compatible server.
type OpenAI struct{ c *Clients }

type openAIModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`           // OpenRouter
	ContextLength int    `json:"context_length"` // OpenRouter, Groq ("context_window" there)
	ContextWindow int    `json:"context_window"`
	MaxContext    int    `json:"max_context_length"` // Mistral
	Architecture  *struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"` // OpenRouter
	Capabilities *struct {
		Vision *bool `json:"vision"`
	} `json:"capabilities"` // Mistral
	TopProvider *struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"` // OpenRouter
}

func (a *OpenAI) ListModels(ctx context.Context, c Credential) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client(a.c).Do(req)
	if err != nil {
		return nil, &Error{Provider: c.Spec.ID, Message: "сервис недоступен: " + scrub(err.Error(), c.Key)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(c.Spec.ID, resp)
	}
	var body struct {
		Data []openAIModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		return nil, &Error{Provider: c.Spec.ID, Message: "не удалось разобрать список моделей"}
	}
	out := make([]Model, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID == "" || (c.Spec.chatOnly != nil && !c.Spec.chatOnly(m.ID)) {
			continue
		}
		mm := Model{Provider: c.Spec.ID, ID: m.ID, DisplayName: m.Name}
		if mm.DisplayName == "" {
			mm.DisplayName = m.ID
		}
		for _, ctxLen := range []int{m.ContextLength, m.ContextWindow, m.MaxContext} {
			if ctxLen > 0 {
				mm.Context = intPtr(ctxLen)
				break
			}
		}
		if m.Architecture != nil {
			mm.Vision = boolPtr(slices.Contains(m.Architecture.InputModalities, "image"))
		} else if m.Capabilities != nil && m.Capabilities.Vision != nil {
			mm.Vision = boolPtr(*m.Capabilities.Vision)
		}
		if m.TopProvider != nil {
			mm.MaxOutput = m.TopProvider.MaxCompletionTokens
		}
		out = append(out, mm)
	}
	return out, nil
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or content parts with images
}

// openAIContent is plain text, or text and image_url parts (data URLs).
func openAIContent(m Message) any {
	if len(m.Attachments) == 0 {
		return m.Content
	}
	parts := make([]map[string]any, 0, len(m.Attachments)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, a := range m.Attachments {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": dataURL(a)}})
	}
	return parts
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	XGroq *struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	} `json:"x_groq"`
	Error json.RawMessage `json:"error"`
}

func (a *OpenAI) StreamChat(ctx context.Context, c Credential, r ChatRequest, onDelta func(string) error) (Usage, error) {
	var usage Usage
	if err := checkAttachments(r.Messages); err != nil {
		return usage, err
	}
	msgs := make([]openAIChatMessage, 0, len(r.Messages)+1)
	if r.System != "" {
		msgs = append(msgs, openAIChatMessage{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		msgs = append(msgs, openAIChatMessage{Role: m.Role, Content: openAIContent(m)})
	}
	body := map[string]any{"model": r.Model, "messages": msgs, "stream": true}
	if r.Temperature != nil {
		body["temperature"] = *r.Temperature
	}
	if r.MaxTokens > 0 {
		body["max_tokens"] = r.MaxTokens
	}
	if c.Spec.streamUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return usage, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.Spec.ID == "openrouter" {
		req.Header.Set("X-Title", "AIHub")
	}
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
	err = ReadSSE(resp.Body, func(_, data string) error {
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			return io.EOF
		}
		var ch openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil // tolerate keep-alive junk
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			msg := extractErrorMessage([]byte(`{"error":` + string(ch.Error) + `}`))
			if msg == "" {
				msg = "ошибка в потоке ответа"
			}
			return &Error{Provider: c.Spec.ID, Message: msg}
		}
		if ch.Usage != nil {
			usage = Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
		} else if ch.XGroq != nil && ch.XGroq.Usage != nil {
			usage = Usage{InputTokens: ch.XGroq.Usage.PromptTokens, OutputTokens: ch.XGroq.Usage.CompletionTokens}
		}
		for _, choice := range ch.Choices {
			if choice.Delta.Content != "" {
				if err := onDelta(choice.Delta.Content); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return usage, ctx.Err()
	}
	return usage, err
}

// scrub removes the key from an error text, in case a transport error ever
// echoes the request.
func scrub(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[key]")
	}
	return s
}
