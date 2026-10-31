package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"odysseia.local/backend/internal/blob"
	"odysseia.local/backend/internal/config"
	"odysseia.local/backend/internal/httpapi"
	"odysseia.local/backend/internal/media"
	"odysseia.local/backend/internal/postgres"
	"odysseia.local/backend/internal/realtime"
	"odysseia.local/backend/internal/tokenauth"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	pool, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	verifier, err := tokenauth.New(ctx, tokenauth.Config{
		Issuer: cfg.AuthIssuer, Audience: cfg.AuthAudience,
		JWKSURL: cfg.AuthJWKSURL, CAFile: cfg.AuthCAFile,
	})
	if err != nil {
		return err
	}
	tokens, err := realtime.NewSigner(cfg.RealtimeConnectionSecret, realtime.TokenTTL)
	if err != nil {
		return err
	}
	forums, err := postgres.NewCachedForums(pool, cfg.MaxWriteTransactions)
	if err != nil {
		return err
	}
	defer forums.Close()
	if err := forums.UseConversationMemberLimit(cfg.ConversationMemberLimit); err != nil {
		return err
	}
	objects, err := blob.NewR2(ctx, blob.R2Config{
		AccountID: cfg.R2AccountID, AccessKey: cfg.R2AccessKeyID, SecretKey: cfg.R2SecretAccessKey,
		Bucket: cfg.R2Bucket, Endpoint: cfg.R2Endpoint, Region: cfg.R2Region,
	})
	if err != nil {
		return err
	}
	limits := media.Limits{
		MaxBytes: cfg.AttachmentMaxBytes, MaxEdge: cfg.AttachmentMaxEdge,
		MaxPixels: cfg.AttachmentMaxPixels, MaxVoiceMillis: cfg.AttachmentMaxVoiceMS,
	}
	if err := forums.UseAttachments(postgres.AttachmentSettings{
		Store: objects, Limits: limits, UploadsPerMinute: cfg.AttachmentUploadsPerMinute,
		QuotaBytes: cfg.AttachmentQuotaBytes, PresignTTL: cfg.AttachmentPresignTTL,
		MaxPerMessage: cfg.AttachmentMaxPerMessage,
	}); err != nil {
		return err
	}
	users := postgres.NewUsers(pool)
	api := httpapi.NewAPI(verifier.Authenticate, users.Ensure)
	if err := api.SetMediaLimits(limits); err != nil {
		return err
	}
	api.RegisterForums(forums)
	api.RegisterChannels(forums)
	api.RegisterConversations(forums)
	api.RegisterCollaboration(postgres.NewCollaboration(pool))
	if err := api.RegisterRealtime(tokens, forums, cfg.RealtimeProxySecret); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.HTTPSAddr)
	if err != nil {
		return fmt.Errorf("listen for HTTPS: %w", err)
	}
	server := httpapi.NewServer(cfg.HTTPSAddr, pool.Ping, api)
	return httpapi.ServeTLS(ctx, server, listener, cfg.TLSCertFile, cfg.TLSKeyFile)
}
