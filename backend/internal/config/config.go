// Package config reads the backend's settings from environment variables.
// Secrets (the master key) come only from the environment, never from files
// in the repository; see .env.example for placeholders.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen    string
	DBPath    string
	MasterKey []byte // 32 bytes
	PublicURL string // https://aihub.example.com, used for OAuth callbacks
	LogLevel  string

	AdminEmail           string
	Registration         string // "open" or "invite"
	TrustProxy           bool
	AllowPrivateBaseURLs bool

	DefaultDailyRequests int64
	DefaultDailyTokens   int64

	// Tunnel (OpenFlux channels).
	TunnelPublicHost string
	TunnelPortMin    int
	TunnelPortMax    int
	TunnelCupsOnline bool
	NodectlMode      string // off, socket, exec
	NodectlSocket    string
	NodectlCommand   []string

	ShutdownTimeout time.Duration
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func getbool(k string, def bool) (bool, error) {
	v := getenv(k, "")
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", k, err)
	}
	return b, nil
}

func getint(k string, def int64) (int64, error) {
	v := getenv(k, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

// ParseMasterKey accepts 64 hex characters or base64 of 32 bytes.
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("AIHUB_MASTER_KEY is not set (generate one: openssl rand -hex 32)")
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("AIHUB_MASTER_KEY must be 32 bytes: 64 hex chars or base64")
}

// Load reads the configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Listen:           getenv("AIHUB_LISTEN", ":8080"),
		DBPath:           getenv("AIHUB_DB_PATH", "aihub.db"),
		PublicURL:        strings.TrimRight(getenv("AIHUB_PUBLIC_URL", "http://localhost:8080"), "/"),
		LogLevel:         getenv("AIHUB_LOG_LEVEL", "info"),
		AdminEmail:       strings.ToLower(getenv("ADMIN_EMAIL", "")),
		Registration:     getenv("AIHUB_REGISTRATION", "open"),
		TunnelPublicHost: getenv("TUNNEL_PUBLIC_HOST", ""),
		NodectlMode:      getenv("NODECTL_MODE", "off"),
		NodectlSocket:    getenv("NODECTL_SOCKET", "/run/aihub-nodectl/nodectl.sock"),
		NodectlCommand:   strings.Fields(getenv("NODECTL_COMMAND", "sudo -n /usr/local/bin/aihub-nodectl")),
		ShutdownTimeout:  15 * time.Second,
	}
	var err error
	if c.MasterKey, err = ParseMasterKey(os.Getenv("AIHUB_MASTER_KEY")); err != nil {
		return nil, err
	}
	if c.TrustProxy, err = getbool("AIHUB_TRUST_PROXY", false); err != nil {
		return nil, err
	}
	if c.AllowPrivateBaseURLs, err = getbool("AIHUB_ALLOW_PRIVATE_BASE_URLS", false); err != nil {
		return nil, err
	}
	if c.TunnelCupsOnline, err = getbool("TUNNEL_CUPSONLINE", true); err != nil {
		return nil, err
	}
	if c.DefaultDailyRequests, err = getint("AIHUB_DEFAULT_DAILY_REQUESTS", 0); err != nil {
		return nil, err
	}
	if c.DefaultDailyTokens, err = getint("AIHUB_DEFAULT_DAILY_TOKENS", 0); err != nil {
		return nil, err
	}
	pmin, err := getint("TUNNEL_PORT_MIN", 20000)
	if err != nil {
		return nil, err
	}
	pmax, err := getint("TUNNEL_PORT_MAX", 20999)
	if err != nil {
		return nil, err
	}
	c.TunnelPortMin, c.TunnelPortMax = int(pmin), int(pmax)
	if c.TunnelPortMin < 1024 || c.TunnelPortMax > 65535 || c.TunnelPortMin > c.TunnelPortMax {
		return nil, errors.New("TUNNEL_PORT_MIN/MAX must be within 1024-65535, min <= max")
	}
	switch c.Registration {
	case "open", "invite":
	default:
		return nil, errors.New("AIHUB_REGISTRATION must be open or invite")
	}
	switch c.NodectlMode {
	case "off", "socket", "exec":
	default:
		return nil, errors.New("NODECTL_MODE must be off, socket or exec")
	}
	if c.NodectlMode == "exec" && len(c.NodectlCommand) == 0 {
		return nil, errors.New("NODECTL_COMMAND is empty")
	}
	if u, err := url.Parse(c.PublicURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("AIHUB_PUBLIC_URL must be an absolute http(s) URL")
	}
	return c, nil
}
