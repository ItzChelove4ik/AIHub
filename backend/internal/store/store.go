// Package store defines AIHub's persistence as an interface. The SQLite
// implementation lives in store/sqlite; another database (Postgres) can be
// added behind the same interface.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

type Device struct {
	ID         int64
	UserID     int64
	DeviceID   string
	Name       string
	CreatedAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

type RefreshToken struct {
	ID         int64
	UserID     int64
	DeviceID   int64 // devices.id
	Hash       string
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *int64
}

type Credential struct {
	ID        int64
	UserID    int64
	Slug      string // what the API calls the provider: "openai", "custom-mylab"
	Provider  string // adapter spec: openai, anthropic, gemini, openrouter, xai, ..., custom
	Label     string
	BaseURL   string
	Kind      string // api_key | oauth
	SealedKey []byte
	Last4     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type OAuthState struct {
	Hash           string
	UserID         int64
	Provider       string
	SealedVerifier []byte
	ExpiresAt      time.Time
}

type Conversation struct {
	ID        int64
	UserID    int64
	Title     string
	Provider  string
	Model     string
	System    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Message struct {
	ID             int64
	ConversationID int64
	Role           string
	Content        string
	Provider       string
	Model          string
	InputTokens    int64
	OutputTokens   int64
	Status         string
	CreatedAt      time.Time
}

type UsageRow struct {
	UserID       int64
	Provider     string
	Day          string
	Requests     int64
	InputTokens  int64
	OutputTokens int64
}

// Limits are a user's daily limits; nil means the server default, 0 means
// unlimited.
type Limits struct {
	DailyRequests *int64
	DailyTokens   *int64
}

type APIKey struct {
	ID         int64
	UserID     int64
	Name       string
	Prefix     string
	Hash       string
	Scopes     []string
	RPM        int
	LastUsedAt *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

type Channel struct {
	ID               int64
	UserID           *int64
	DeviceID         *int64 // devices.id
	Channel          string
	Port             int
	TransportTypes   string
	SealedTransports []byte
	InviteHash       *string
	Status           string
	CreatedAt        time.Time
}

type Carrier struct {
	ID        int64
	Type      string
	SealedURL []byte
	URLHash   string
	Hint      string
	Status    string
	ChannelID *int64
	CreatedAt time.Time
}

type ConversationQuery struct {
	Search string
	Limit  int
	Before int64 // conversation id cursor (updated_at order), 0 = first page
}

// Store is everything the backend persists.
type Store interface {
	Close() error
	Ping(ctx context.Context) error

	// Users and devices.
	CountUsers(ctx context.Context) (int64, error)
	CreateUser(ctx context.Context, u *User) error
	UserByEmail(ctx context.Context, email string) (*User, error)
	UserByID(ctx context.Context, id int64) (*User, error)
	ListUsers(ctx context.Context) ([]User, error)
	UpsertDevice(ctx context.Context, userID int64, deviceID, name string, now time.Time) (*Device, error)
	DeviceByID(ctx context.Context, id int64) (*Device, error)
	ListDevices(ctx context.Context, userID int64) ([]Device, error)
	RevokeDevice(ctx context.Context, userID, id int64, now time.Time) error
	TouchDevice(ctx context.Context, id int64, now time.Time) error

	// Refresh tokens.
	CreateRefreshToken(ctx context.Context, t *RefreshToken) error
	RefreshTokenByHash(ctx context.Context, hash string) (*RefreshToken, error)
	// RotateRefreshToken revokes old (only if it is still live) and stores
	// next, atomically; ErrNotFound if old was already revoked.
	RotateRefreshToken(ctx context.Context, oldID int64, next *RefreshToken, now time.Time) error
	RevokeDeviceTokens(ctx context.Context, deviceID int64, now time.Time) error

	// Provider credentials.
	UpsertCredential(ctx context.Context, c *Credential) error
	ListCredentials(ctx context.Context, userID int64) ([]Credential, error)
	CredentialBySlug(ctx context.Context, userID int64, slug string) (*Credential, error)
	DeleteCredential(ctx context.Context, userID, id int64) error

	// OAuth PKCE states.
	CreateOAuthState(ctx context.Context, s *OAuthState) error
	// TakeOAuthState returns and deletes a live state.
	TakeOAuthState(ctx context.Context, hash string, now time.Time) (*OAuthState, error)

	// Conversations.
	CreateConversation(ctx context.Context, c *Conversation) error
	ConversationByID(ctx context.Context, userID, id int64) (*Conversation, error)
	ListConversations(ctx context.Context, userID int64, q ConversationQuery) ([]Conversation, error)
	UpdateConversation(ctx context.Context, c *Conversation) error
	DeleteConversation(ctx context.Context, userID, id int64) error
	AddMessage(ctx context.Context, m *Message) error
	ListMessages(ctx context.Context, conversationID int64) ([]Message, error)

	// Usage and limits.
	AddUsage(ctx context.Context, userID int64, provider, day string, requests, in, out int64) error
	UsageForDay(ctx context.Context, userID int64, day string) (requests, tokens int64, err error)
	ListUsage(ctx context.Context, userID int64, fromDay string) ([]UsageRow, error)
	GetLimits(ctx context.Context, userID int64) (Limits, error)
	SetLimits(ctx context.Context, userID int64, l Limits) error

	// AIHub API keys.
	CreateAPIKey(ctx context.Context, k *APIKey) error
	ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error)
	APIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	RevokeAPIKey(ctx context.Context, userID, id int64, now time.Time) error
	TouchAPIKey(ctx context.Context, id int64, now time.Time) error

	// Tunnel channels and the carrier pool.
	CreateChannel(ctx context.Context, c *Channel) error
	ChannelByID(ctx context.Context, id int64) (*Channel, error)
	ChannelByInvite(ctx context.Context, inviteHash string) (*Channel, error)
	ActiveChannelForDevice(ctx context.Context, deviceID int64) (*Channel, error)
	ListChannels(ctx context.Context, userID *int64) ([]Channel, error)
	BindChannel(ctx context.Context, id, userID, deviceID int64) error
	SetChannelStatus(ctx context.Context, id int64, status string) error
	UsedPorts(ctx context.Context) (map[int]bool, error)

	AddCarrier(ctx context.Context, c *Carrier) error
	ListCarriers(ctx context.Context) ([]Carrier, error)
	DeleteCarrier(ctx context.Context, id int64) error
	SetCarrierStatus(ctx context.Context, id int64, status string) error
	// ReserveCarrier marks one free carrier of the type assigned and
	// returns it; ErrNotFound if the pool has none.
	ReserveCarrier(ctx context.Context, typ string) (*Carrier, error)
	AttachCarrier(ctx context.Context, carrierID, channelID int64) error
	ReleaseCarrier(ctx context.Context, carrierID int64) error
	ReleaseChannelCarriers(ctx context.Context, channelID int64) error
	CarriersForChannel(ctx context.Context, channelID int64) ([]Carrier, error)
}
