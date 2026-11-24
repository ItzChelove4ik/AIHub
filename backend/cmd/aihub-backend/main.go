// Command aihub-backend is the AIHub server: accounts, provider keys, the
// unified chat API, the OpenAI-compatible API and OpenFlux channel
// management.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/apikeys"
	"github.com/ItzChelove4ik/AIHub/backend/internal/auth"
	"github.com/ItzChelove4ik/AIHub/backend/internal/chat"
	"github.com/ItzChelove4ik/AIHub/backend/internal/config"
	"github.com/ItzChelove4ik/AIHub/backend/internal/connections"
	"github.com/ItzChelove4ik/AIHub/backend/internal/httpapi"
	"github.com/ItzChelove4ik/AIHub/backend/internal/nodectl"
	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store/sqlite"
	"github.com/ItzChelove4ik/AIHub/backend/internal/tunnel"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	log := newLogger(cfg.LogLevel)
	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func run(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	box, err := secure.NewBox(cfg.MasterKey)
	if err != nil {
		return err
	}

	clients := providers.NewClients(cfg.AllowPrivateBaseURLs)
	conn := connections.New(db, box, providers.NewRegistry(clients), cfg.AllowPrivateBaseURLs)
	authSvc := auth.New(db, cfg.MasterKey, cfg.AdminEmail, cfg.Registration)

	var ctl nodectl.Client
	switch cfg.NodectlMode {
	case "socket":
		ctl = &nodectl.SocketClient{Path: cfg.NodectlSocket}
	case "exec":
		ctl = &nodectl.ExecClient{Command: cfg.NodectlCommand}
	}
	tun := tunnel.New(db, box, ctl, tunnel.Options{PublicHost: cfg.TunnelPublicHost, PortMin: cfg.TunnelPortMin,
		PortMax: cfg.TunnelPortMax, CupsOnline: cfg.TunnelCupsOnline}, log)
	authSvc.SetInviteBinder(tun)

	srv := &httpapi.Server{
		Store:      db,
		Auth:       authSvc,
		Conn:       conn,
		OAuth:      connections.NewOpenRouterOAuth(conn, cfg.PublicURL, clients.Trusted),
		Chat:       chat.New(db, conn, log, cfg.DefaultDailyRequests, cfg.DefaultDailyTokens),
		APIKeys:    apikeys.New(db),
		Tunnel:     tun,
		Log:        log,
		TrustProxy: cfg.TrustProxy,
		Version:    version,
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Generous: a photo through a document carrier uploads slowly.
		ReadTimeout: 10 * time.Minute,
		IdleTimeout: 120 * time.Second,
		// No WriteTimeout: chat answers stream for minutes. Streams end
		// when the client disconnects or the provider finishes.
		MaxHeaderBytes: 64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", version, "tunnel", tun.Enabled(), "registration", cfg.Registration)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpSrv.Shutdown(sctx)
}

// healthcheck is for the Docker HEALTHCHECK (the image has no curl).
func healthcheck() int {
	addr := os.Getenv("AIHUB_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/readyz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 1
	}
	return 0
}
