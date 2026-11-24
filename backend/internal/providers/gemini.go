package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Gemini speaks the Gemini API (generativelanguage.googleapis.com v1beta).
type Gemini struct{ c *Clients }

func (g *Gemini) ListModels(ctx context.Context, c Credential) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out []Model
	token := ""
	for page := 0; page < 20; page++ {
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", c.Key)
		resp, err := c.client(g.c).Do(req)
		if err != nil {
			return nil, &Error{Provider: c.Spec.ID, Message: "сервис недоступен: " + scrub(err.Error(), c.Key)}
		}
		if resp.StatusCode != http.StatusOK {
			err := readError(c.Spec.ID, resp)
			resp.Body.Close()
			return nil, err
		}
		var body struct {
			Models []struct {
				Name                       string   `json:"name"`
				DisplayName                string   `json:"displayName"`
				InputTokenLimit            int      `json:"inputTokenLimit"`
				OutputTokenLimit           int      `json:"outputTokenLimit"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, &Error{Provider: c.Spec.ID, Message: "не удалось разобрать список моделей"}
		}
		for _, m := range body.Models {
			if !slices.Contains(m.SupportedGenerationMethods, "generateContent") {
				continue
			}
			id := strings.TrimPrefix(m.Name, "models/")
			mm := Model{Provider: c.Spec.ID, ID: id, DisplayName: m.DisplayName, MaxOutput: m.OutputTokenLimit}
			if mm.DisplayName == "" {
				mm.DisplayName = id
			}
			if m.InputTokenLimit > 0 {
				mm.Context = intPtr(m.InputTokenLimit)
			}
			out = append(out, mm)
		}
		if body.NextPageToken == "" {
			break
		}
		token = body.NextPageToken
	}
	return out, nil
}

type geminiPart struct {
	Text       string      `json:"text,omitempty"`
	Thought    bool        `json:"thought,omitempty"`
	InlineData *geminiBlob `json:"inlineData,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

func (g *Gemini) StreamChat(ctx context.Context, c Credential, r ChatRequest, onDelta func(string) error) (Usage, error) {
	var usage Usage
	if err := checkAttachments(r.Messages); err != nil {
		return usage, err
	}
	contents := make([]geminiContent, 0, len(r.Messages))
	for _, m := range r.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		parts := make([]geminiPart, 0, len(m.Attachments)+1)
		for _, a := range m.Attachments {
			parts = append(parts, geminiPart{InlineData: &geminiBlob{MimeType: a.MediaType, Data: a.Data}})
		}
		if m.Content != "" || len(parts) == 0 {
			parts = append(parts, geminiPart{Text: m.Content})
		}
		contents = append(contents, geminiContent{Role: role, Parts: parts})
	}
	body := map[string]any{"contents": contents}
	if r.System != "" {
		body["systemInstruction"] = geminiContent{Parts: []geminiPart{{Text: r.System}}}
	}
	gen := map[string]any{}
	if r.Temperature != nil {
		gen["temperature"] = *r.Temperature
	}
	if r.MaxTokens > 0 {
		gen["maxOutputTokens"] = r.MaxTokens
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	raw, _ := json.Marshal(body)
	endpoint := c.base() + "/models/" + url.PathEscape(r.Model) + ":streamGenerateContent?alt=sse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return usage, err
	}
	req.Header.Set("x-goog-api-key", c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.client(g.c).Do(req)
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
		var ch struct {
			Candidates []struct {
				Content geminiContent `json:"content"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int64 `json:"promptTokenCount"`
				CandidatesTokenCount int64 `json:"candidatesTokenCount"`
				ThoughtsTokenCount   int64 `json:"thoughtsTokenCount"`
			} `json:"usageMetadata"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil
		}
		if ch.Error != nil {
			return &Error{Provider: c.Spec.ID, Message: ch.Error.Message}
		}
		if ch.UsageMetadata != nil {
			usage = Usage{InputTokens: ch.UsageMetadata.PromptTokenCount,
				OutputTokens: ch.UsageMetadata.CandidatesTokenCount + ch.UsageMetadata.ThoughtsTokenCount}
		}
		for _, cand := range ch.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" && !p.Thought {
					if err := onDelta(p.Text); err != nil {
						return err
					}
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
