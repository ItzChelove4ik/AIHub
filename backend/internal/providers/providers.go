// Package providers talks to AI providers behind one interface: list the
// models a key can use, and stream a chat completion. Model lists always
// come from the provider's API, never from a hard-coded list.
package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Kind is the wire protocol an adapter speaks.
type Kind string

const (
	KindOpenAI    Kind = "openai" // OpenAI Chat Completions and compatible APIs
	KindAnthropic Kind = "anthropic"
	KindGemini    Kind = "gemini"
)

// Spec describes one provider.
type Spec struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    Kind   `json:"kind"`
	BaseURL string `json:"base_url,omitempty"`
	// Custom providers take the base URL from the user.
	Custom bool `json:"custom,omitempty"`
	// OAuth: the credential can come from "Sign in with ..." instead of a key.
	OAuth bool `json:"oauth,omitempty"`
	// streamUsage: send stream_options.include_usage (OpenAI kind).
	streamUsage bool
	// chatOnly filters a model list down to chat models, for providers
	// whose list mixes in embeddings, audio and image models.
	chatOnly func(id string) bool
}

func openAIChatOnly(id string) bool {
	id = strings.ToLower(id)
	for _, bad := range []string{"embedding", "tts", "whisper", "dall-e", "moderation", "transcribe", "realtime", "audio", "image", "davinci", "babbage", "sora"} {
		if strings.Contains(id, bad) {
			return false
		}
	}
	return true
}

// Specs are the supported providers. Base URLs were checked against each
// provider's API documentation.
var Specs = map[string]Spec{
	"openai":     {ID: "openai", Name: "OpenAI", Kind: KindOpenAI, BaseURL: "https://api.openai.com/v1", streamUsage: true, chatOnly: openAIChatOnly},
	"anthropic":  {ID: "anthropic", Name: "Anthropic", Kind: KindAnthropic, BaseURL: "https://api.anthropic.com/v1"},
	"gemini":     {ID: "gemini", Name: "Google Gemini", Kind: KindGemini, BaseURL: "https://generativelanguage.googleapis.com/v1beta"},
	"openrouter": {ID: "openrouter", Name: "OpenRouter", Kind: KindOpenAI, BaseURL: "https://openrouter.ai/api/v1", streamUsage: true, OAuth: true},
	"xai":        {ID: "xai", Name: "xAI", Kind: KindOpenAI, BaseURL: "https://api.x.ai/v1", streamUsage: true, chatOnly: openAIChatOnly},
	"deepseek":   {ID: "deepseek", Name: "DeepSeek", Kind: KindOpenAI, BaseURL: "https://api.deepseek.com", streamUsage: true},
	"mistral":    {ID: "mistral", Name: "Mistral", Kind: KindOpenAI, BaseURL: "https://api.mistral.ai/v1", chatOnly: openAIChatOnly},
	"groq":       {ID: "groq", Name: "Groq", Kind: KindOpenAI, BaseURL: "https://api.groq.com/openai/v1", streamUsage: true, chatOnly: openAIChatOnly},
	"custom":     {ID: "custom", Name: "OpenAI-compatible", Kind: KindOpenAI, Custom: true},
}

// Model is one entry of the model catalog.
type Model struct {
	Provider    string `json:"provider"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Context     *int   `json:"context"`
	Vision      *bool  `json:"vision"`
	// MaxOutput is the provider's output token cap, when it says.
	MaxOutput int `json:"-"`
}

// Attachment is a file sent with a message: an image, base64 encoded.
type Attachment struct {
	Type      string `json:"type"`       // image
	MediaType string `json:"media_type"` // image/jpeg, image/png, image/webp, image/gif
	Data      string `json:"data"`       // standard base64
}

// Image limits: what every supported provider accepts inline (Anthropic's
// 5 MB per image is the tightest).
const (
	MaxImageBytes       = 5 << 20
	MaxImagesPerMsg     = 10
	MaxImagesPerRequest = 20
	// MaxImageBytesPerRequest keeps a request under Gemini's 20 MB inline cap.
	MaxImageBytesPerRequest = 18 << 20
)

// ImageMediaTypes are the image formats the providers take inline.
var ImageMediaTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true}

// ValidateAttachment checks one attachment and returns its decoded bytes.
func ValidateAttachment(a Attachment) ([]byte, error) {
	if a.Type != "image" {
		return nil, errors.New("поддерживаются только вложения-изображения")
	}
	if !ImageMediaTypes[a.MediaType] {
		return nil, errors.New("формат изображения: jpeg, png, webp или gif")
	}
	if base64.StdEncoding.DecodedLen(len(a.Data)) > MaxImageBytes+3 {
		return nil, fmt.Errorf("изображение больше %d МБ", MaxImageBytes>>20)
	}
	raw, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		return nil, errors.New("изображение должно быть в base64")
	}
	if len(raw) == 0 || len(raw) > MaxImageBytes {
		return nil, fmt.Errorf("изображение пустое или больше %d МБ", MaxImageBytes>>20)
	}
	if got := http.DetectContentType(raw); got != a.MediaType {
		return nil, errors.New("содержимое изображения не совпадает с media_type")
	}
	return raw, nil
}

// HasImages reports whether any message carries an image.
func HasImages(msgs []Message) bool {
	for _, m := range msgs {
		if len(m.Attachments) > 0 {
			return true
		}
	}
	return false
}

func dataURL(a Attachment) string { return "data:" + a.MediaType + ";base64," + a.Data }

type Message struct {
	Role        string       `json:"role"` // user | assistant
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

type ChatRequest struct {
	Model       string
	System      string
	Messages    []Message
	Temperature *float64
	MaxTokens   int
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Credential is a decrypted key ready for one request.
type Credential struct {
	Spec    Spec
	Key     string
	BaseURL string // overrides Spec.BaseURL (custom providers)
}

func (c Credential) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return c.Spec.BaseURL
}

// Adapter is one wire protocol.
type Adapter interface {
	ListModels(ctx context.Context, c Credential) ([]Model, error)
	// StreamChat calls onDelta with each piece of text as it arrives and
	// returns the token usage the provider reported.
	StreamChat(ctx context.Context, c Credential, req ChatRequest, onDelta func(string) error) (Usage, error)
}

var ErrAttachmentsUnsupported = errors.New("неподдерживаемые или слишком многочисленные вложения")

// Error is an error the provider returned. Message never contains the key.
type Error struct {
	Provider string
	Status   int
	Message  string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Message)
	}
	return e.Provider + ": " + e.Message
}

// Unauthorized reports whether the provider rejected the key.
func (e *Error) Unauthorized() bool { return e.Status == 401 || e.Status == 403 }

// Clients holds the HTTP clients adapters use. Every outgoing request has
// connect, TLS and response-header timeouts; streams are bounded by the
// request context.
type Clients struct {
	Trusted   *http.Client // known provider hosts
	Untrusted *http.Client // user-supplied base URLs: private addresses refused
}

func NewClients(allowPrivate bool) *Clients {
	return &Clients{Trusted: newClient(false), Untrusted: newClient(!allowPrivate)}
}

func newClient(blockPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if blockPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
				ip.IsUnspecified() || ip.IsMulticast() || isCGNAT(ip) {
				return fmt.Errorf("address %s is not allowed for a custom provider", host)
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // reasoning models can think before the first byte
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

func isCGNAT(ip net.IP) bool {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

// Registry picks the adapter for a credential.
type Registry struct {
	clients   *Clients
	openai    *OpenAI
	anthropic *Anthropic
	gemini    *Gemini
}

func NewRegistry(c *Clients) *Registry {
	return &Registry{clients: c, openai: &OpenAI{c: c}, anthropic: &Anthropic{c: c}, gemini: &Gemini{c: c}}
}

func (r *Registry) Adapter(spec Spec) Adapter {
	switch spec.Kind {
	case KindAnthropic:
		return r.anthropic
	case KindGemini:
		return r.gemini
	default:
		return r.openai
	}
}

// ValidateBaseURL checks a custom provider's base URL.
func ValidateBaseURL(raw string, allowHTTP bool) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base_url должен быть полным URL вида https://host/v1")
	}
	if u.Scheme != "https" && (!allowHTTP || u.Scheme != "http") {
		return "", errors.New("base_url должен начинаться с https://")
	}
	return raw, nil
}

func (c Credential) client(cl *Clients) *http.Client {
	if c.Spec.Custom {
		return cl.Untrusted
	}
	return cl.Trusted
}

// readError turns an error response into an *Error, with the provider's
// own message when it has one.
func readError(provider string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := extractErrorMessage(body)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &Error{Provider: provider, Status: resp.StatusCode, Message: msg}
}

func extractErrorMessage(body []byte) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &v) == nil {
		if len(v.Error) > 0 {
			var obj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(v.Error, &obj) == nil && obj.Message != "" {
				return truncate(obj.Message)
			}
			var s string
			if json.Unmarshal(v.Error, &s) == nil && s != "" {
				return truncate(s)
			}
		}
		if v.Message != "" {
			return truncate(v.Message)
		}
	}
	return ""
}

func truncate(s string) string {
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

// checkAttachments is the adapters' last line of defence; chat and the
// OpenAI-compatible API validate earlier with friendlier errors.
func checkAttachments(msgs []Message) error {
	total := 0
	for _, m := range msgs {
		if len(m.Attachments) > MaxImagesPerMsg {
			return ErrAttachmentsUnsupported
		}
		for _, a := range m.Attachments {
			if a.Type != "image" || !ImageMediaTypes[a.MediaType] || a.Data == "" {
				return ErrAttachmentsUnsupported
			}
			total++
		}
	}
	if total > MaxImagesPerRequest {
		return ErrAttachmentsUnsupported
	}
	return nil
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }
