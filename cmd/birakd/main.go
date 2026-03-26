package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/birak/birak/internal/config"
	httpuigw "github.com/birak/birak/internal/gateway/httpui"
	s3gw "github.com/birak/birak/internal/gateway/s3"
	sftpgw "github.com/birak/birak/internal/gateway/sftp"
	webdavgw "github.com/birak/birak/internal/gateway/webdav"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/syncer"
	"github.com/birak/birak/internal/watcher"
)

func main() {
	configPath := flag.String("config", "", "path to config file (optional, can use env vars instead)")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	// Load configuration.
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Set up structured logger with node_id.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})).With("node", cfg.NodeID)

	logger.Info("starting birak daemon",
		"sync_dir", cfg.SyncDir,
		"listen_addr", cfg.ListenAddr,
		"peers", cfg.Peers,
	)

	// Ensure meta directory exists.
	if err := os.MkdirAll(cfg.MetaDir, 0o755); err != nil {
		return fmt.Errorf("create meta dir: %w", err)
	}

	// Ensure sync directory exists.
	if err := os.MkdirAll(cfg.SyncDir, 0o755); err != nil {
		return fmt.Errorf("create sync dir: %w", err)
	}

	// Open store.
	dbPath := filepath.Join(cfg.MetaDir, "birak.db")
	st, err := store.New(dbPath, logger)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	logger.Info("store opened", "path", dbPath)

	// Create context with cancellation on signals.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Set up watcher with onChange callback that updates the store.
	onChange := func(events []watcher.FileEvent) {
		for _, ev := range events {
			_, err := st.PutFile(ev.Name, ev.ModTime, ev.Size, ev.Hash, ev.Deleted)
			if err != nil {
				logger.Error("store update failed", "name", ev.Name, "error", err)
			}
		}
	}

	w := watcher.New(
		cfg.SyncDir,
		st,
		logger,
		cfg.Sync.DebounceWindow,
		cfg.Sync.ScanInterval,
		cfg.Ignore,
		onChange,
	)

	// Set up syncer.
	syn := syncer.New(
		st,
		w,
		cfg.SyncDir,
		cfg.NodeID,
		cfg.Peers,
		cfg.Ignore,
		logger,
		cfg.Sync.PollInterval,
		cfg.Sync.BatchLimit,
		cfg.Sync.MaxConcurrentDownloads,
	)

	// Set up HTTP server.
	srv := server.New(st, cfg.SyncDir, cfg.NodeID, cfg.Ignore, logger)
	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Handler(),
	}

	// Launch all components.
	var wg sync.WaitGroup

	// HTTP server.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("HTTP server starting", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
			cancel()
		}
	}()

	// Watcher.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := w.Run(ctx); err != nil {
			logger.Error("watcher error", "error", err)
			cancel()
		}
	}()

	// Syncer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		syn.Run(ctx)
	}()

	// Tombstone purger.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				purged, err := st.PurgeTombstones(cfg.Sync.TombstoneTTL)
				if err != nil {
					logger.Error("tombstone purge failed", "error", err)
				} else if purged > 0 {
					logger.Info("tombstones purged", "count", purged)
				}
			}
		}
	}()

	// S3 Gateway (if enabled).
	var s3Gateway *s3gw.Gateway
	if cfg.Gateways.S3.Enabled {
		s3Gateway = s3gw.New(
			cfg.SyncDir,
			cfg.Ignore,
			s3gw.Config{
				ListenAddr: cfg.Gateways.S3.ListenAddr,
				AccessKey:  cfg.Gateways.S3.AccessKey,
				SecretKey:  cfg.Gateways.S3.SecretKey,
				Domain:     cfg.Gateways.S3.Domain,
			},
			logger,
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s3Gateway.Start(ctx); err != nil {
				logger.Error("S3 gateway error", "error", err)
				cancel()
			}
		}()
	}

	// WebDAV Gateway (if enabled).
	var webdavGateway *webdavgw.Gateway
	if cfg.Gateways.WebDAV.Enabled {
		webdavGateway = webdavgw.New(
			cfg.SyncDir,
			cfg.Ignore,
			webdavgw.Config{
				ListenAddr: cfg.Gateways.WebDAV.ListenAddr,
				Username:   cfg.Gateways.WebDAV.Username,
				Password:   cfg.Gateways.WebDAV.Password,
			},
			logger,
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := webdavGateway.Start(ctx); err != nil {
				logger.Error("WebDAV gateway error", "error", err)
				cancel()
			}
		}()
	}

	// HTTP file server Gateway (if enabled).
	var httpGateway *httpuigw.Gateway
	if cfg.Gateways.HTTP.Enabled {
		httpGateway = httpuigw.New(
			cfg.SyncDir,
			cfg.Ignore,
			httpuigw.Config{
				ListenAddr: cfg.Gateways.HTTP.ListenAddr,
				Username:   cfg.Gateways.HTTP.Username,
				Password:   cfg.Gateways.HTTP.Password,
			},
			logger,
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := httpGateway.Start(ctx); err != nil {
				logger.Error("HTTP file server error", "error", err)
				cancel()
			}
		}()
	}

	// SFTP Gateway (if enabled).
	var sftpGateway *sftpgw.Gateway
	if cfg.Gateways.SFTP.Enabled {
		sftpGateway, err = sftpgw.New(
			cfg.SyncDir,
			cfg.Ignore,
			cfg.MetaDir,
			sftpgw.Config{
				ListenAddr:  cfg.Gateways.SFTP.ListenAddr,
				Username:    cfg.Gateways.SFTP.Username,
				Password:    cfg.Gateways.SFTP.Password,
				HostKeyPath: cfg.Gateways.SFTP.HostKeyPath,
			},
			logger,
		)
		if err != nil {
			return fmt.Errorf("sftp gateway init: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sftpGateway.Start(ctx); err != nil {
				logger.Error("SFTP gateway error", "error", err)
				cancel()
			}
		}()
	}

	// Wait for shutdown signal.
	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig)
	case <-ctx.Done():
	}

	cancel()

	// Graceful HTTP shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}

	// Graceful S3 gateway shutdown.
	if s3Gateway != nil {
		if err := s3Gateway.Stop(shutdownCtx); err != nil {
			logger.Error("S3 gateway shutdown error", "error", err)
		}
	}

	// Graceful WebDAV gateway shutdown.
	if webdavGateway != nil {
		if err := webdavGateway.Stop(shutdownCtx); err != nil {
			logger.Error("WebDAV gateway shutdown error", "error", err)
		}
	}

	// Graceful HTTP file server shutdown.
	if httpGateway != nil {
		if err := httpGateway.Stop(shutdownCtx); err != nil {
			logger.Error("HTTP file server shutdown error", "error", err)
		}
	}

	// Graceful SFTP gateway shutdown.
	if sftpGateway != nil {
		if err := sftpGateway.Stop(shutdownCtx); err != nil {
			logger.Error("SFTP gateway shutdown error", "error", err)
		}
	}

	wg.Wait()
	logger.Info("birak daemon stopped")
	return nil
}
