package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/gateway"
	httpuigw "github.com/birak/birak/internal/gateway/httpui"
	s3gw "github.com/birak/birak/internal/gateway/s3"
	sftpgw "github.com/birak/birak/internal/gateway/sftp"
	webdavgw "github.com/birak/birak/internal/gateway/webdav"
	"github.com/birak/birak/internal/logging"
	"github.com/birak/birak/internal/multipart"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/syncer"
	"github.com/birak/birak/internal/watcher"
)

// version is the release this binary was built from.
var version = "2.1.2"

// shutdownDrain is how long open transfers may finish after a stop signal.
// It stays below Docker's default 10 s grace period, so a stop with a slow
// download in flight ends in an orderly exit rather than SIGKILL.
const shutdownDrain = 8 * time.Second

func main() {
	configPath := flag.String("config", "", "path to config file (optional, can use env vars instead)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("birakd", version)
		return
	}
	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// Tombstones are retained indefinitely. Stream cursors acknowledge receipt,
// not application, and cannot safely authorize distributed deletion GC.

func run(configPath string) error {
	// Load configuration.
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Set up structured logger with node_id.
	// Writing is asynchronous and bounded: a log consumer that stops reading
	// drops records instead of stopping the goroutine that logged them, which
	// is routinely one holding the volume's commit lock.
	logs := logging.NewAsync(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	defer logs.Close()
	logger := slog.New(logs).With("node", cfg.NodeID)

	gateway.SetAccessLog(cfg.AccessLog)

	logger.Info("starting birak daemon",
		"version", version,
		"sync_dir", cfg.SyncDir,
		"listen_addr", cfg.ListenAddr,
		"peers", cfg.Peers,
	)

	// Ensure meta directory exists.
	if err := os.MkdirAll(cfg.MetaDir, 0o755); err != nil {
		return fmt.Errorf("create meta dir: %w", err)
	}

	releaseMeta, err := fileops.AcquireLease(cfg.MetaDir)
	if err != nil {
		return err
	}
	defer releaseMeta()

	// Ensure sync directory exists.
	if err := os.MkdirAll(cfg.SyncDir, 0o755); err != nil {
		return fmt.Errorf("create sync dir: %w", err)
	}

	private := filepath.Join(cfg.SyncDir, ".birak")
	if info, err := os.Lstat(private); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("storage state directory must be a real directory")
	}
	if err := os.MkdirAll(private, 0o700); err != nil {
		return err
	}
	metaInfo, _ := os.Stat(cfg.MetaDir)
	volumeInfo, _ := os.Stat(private)
	if metaInfo == nil || volumeInfo == nil || !os.SameFile(metaInfo, volumeInfo) {
		releaseVolume, err := fileops.AcquireLease(private)
		if err != nil {
			return err
		}
		defer releaseVolume()
	}

	// Warn about allowed settings that expose data.
	for _, warning := range config.SecurityWarnings(cfg) {
		logger.Warn("insecure configuration", "detail", warning)
	}

	// Unreadable TLS material is a configuration error, reported before any
	// storage is opened rather than as a listener failure later.
	if cfg.TLSCertFile != "" {
		if _, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil {
			return fmt.Errorf("load peer TLS certificate: %w", err)
		}
	}
	var peerRoots *x509.CertPool
	if cfg.PeerCAFile != "" {
		if peerRoots, err = loadPeerRoots(cfg.PeerCAFile); err != nil {
			return err
		}
	}

	// Open store.
	dbPath := filepath.Join(cfg.MetaDir, "birak.db")
	st, err := store.New(dbPath, logger)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	st.SetRepairLimit(cfg.Sync.MaxRepairQueue)

	logger.Info("store opened", "path", dbPath)

	// Create context with cancellation on signals.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// The watcher owns indexing and participates in the shared commit lock.
	w := watcher.New(
		cfg.SyncDir,
		st,
		logger,
		cfg.Sync.DebounceWindow,
		cfg.Sync.ScanInterval,
		cfg.Ignore,
	)

	w.SetScrubRate(cfg.Sync.ScrubBytesPerSecond)
	w.SetMaxExternalDeletions(cfg.Sync.MaxExternalDeletions)

	// Validate the volume and recover interrupted replacements before cleanup,
	// indexing, or exposing any gateway.
	if err := w.CheckStorage(); err != nil {
		return fmt.Errorf("recover storage: %w", err)
	}

	// Forget stream positions for removed peers. Accepted repair work remains
	// durable and visible even when its source is temporarily unconfigured.
	if err := st.PruneCursors(cfg.Peers); err != nil {
		logger.Error("prune stale peer state failed", "error", err)
	}

	// Set up syncer.
	syn := syncer.New(
		st,
		w,
		cfg.SyncDir,
		cfg.NodeID,
		cfg.Peers,
		cfg.Ignore,
		logger,
		syncer.Options{
			PollInterval:           cfg.Sync.PollInterval,
			BatchLimit:             cfg.Sync.BatchLimit,
			MaxConcurrentDownloads: cfg.Sync.MaxConcurrentDownloads,
			RepairInterval:         cfg.Sync.RepairInterval,
			ReconcileInterval:      cfg.Sync.ReconcileInterval,
			ReconcilePageBudget:    cfg.Sync.ReconcilePageBudget,
			Secret:                 cfg.ClusterSecret,
			RootCAs:                peerRoots,
		},
	)
	// The syncer has adopted the partial downloads it can resume by now, so
	// this removes every other scratch file a previous process left behind.
	gateway.SweepTempFiles(cfg.SyncDir, 0, logger)

	// Set up HTTP server. The syncer is attached afterwards so /status can
	// report live per-peer replication state.
	srv := server.New(st, cfg.SyncDir, cfg.NodeID, cfg.Ignore, server.Config{
		Secret:       cfg.ClusterSecret,
		MetricsToken: cfg.MetricsToken,
		MetaDir:      cfg.MetaDir,
		Version:      version,
	}, logger)
	srv.SetStats(syn)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	// Construct every component before starting any goroutine. A later gateway
	// initialization error must not close the store under an active watcher.
	type component struct {
		name string
		run  func(context.Context) error
	}
	components := []component{
		{"HTTP server", func(context.Context) error {
			logger.Info("HTTP server starting", "addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
			var err error
			if cfg.TLSCertFile != "" {
				err = httpServer.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
			} else {
				err = httpServer.ListenAndServe()
			}
			if err != http.ErrServerClosed {
				return err
			}
			return nil
		}},
		{"watcher", w.Run},
		{"syncer", func(ctx context.Context) error { syn.Run(ctx); return nil }},
	}

	// S3 Gateway (if enabled).
	var s3Gateway *s3gw.Gateway
	var multipartStore *multipart.Store
	if cfg.Gateways.S3.Enabled {
		multipartStore, err = multipart.New(cfg.SyncDir, multipart.Limits{
			MinPartBytes:       cfg.Multipart.MinPartBytes,
			MaxPartBytes:       cfg.Multipart.MaxPartBytes,
			MaxParts:           cfg.Multipart.MaxParts,
			MaxObjectBytes:     cfg.MaxUploadBytes,
			MaxActiveUploads:   cfg.Multipart.MaxActiveUploads,
			MaxConcurrentParts: cfg.Multipart.MaxConcurrentPartUploads,
			UploadTTL:          cfg.Multipart.UploadTTL,
			CleanupInterval:    cfg.Multipart.CleanupInterval,
			TempFileMaxAge:     cfg.Multipart.TempFileMaxAge,
		}, logger)
		if err != nil {
			return fmt.Errorf("multipart store init: %w", err)
		}

		s3Gateway = s3gw.New(
			cfg.SyncDir,
			cfg.Ignore,
			s3gw.Config{
				ListenAddr:     cfg.Gateways.S3.ListenAddr,
				AccessKey:      cfg.Gateways.S3.AccessKey,
				SecretKey:      cfg.Gateways.S3.SecretKey,
				Domain:         cfg.Gateways.S3.Domain,
				MaxUploadBytes: cfg.MaxUploadBytes,
				Multipart:      multipartStore,
				TLSCertFile:    cfg.Gateways.S3.TLSCertFile,
				TLSKeyFile:     cfg.Gateways.S3.TLSKeyFile,
				Catalog:        st,
			},
			logger,
		)
		if err := s3Gateway.CreateBuckets(cfg.Gateways.S3.Buckets); err != nil {
			return fmt.Errorf("create buckets: %w", err)
		}
		components = append(components, component{"S3 gateway", s3Gateway.Start})

		components = append(components, component{"multipart janitor", func(ctx context.Context) error {
			multipart.NewJanitor(multipartStore, logger).Run(ctx)
			return nil
		}})
	}

	// WebDAV Gateway (if enabled).
	var webdavGateway *webdavgw.Gateway
	if cfg.Gateways.WebDAV.Enabled {
		webdavGateway = webdavgw.New(
			cfg.SyncDir,
			cfg.Ignore,
			webdavgw.Config{
				ListenAddr:     cfg.Gateways.WebDAV.ListenAddr,
				Username:       cfg.Gateways.WebDAV.Username,
				Password:       cfg.Gateways.WebDAV.Password,
				MaxUploadBytes: cfg.MaxUploadBytes,
			},
			logger,
		)
		components = append(components, component{"WebDAV gateway", webdavGateway.Start})
	}

	// HTTP file server Gateway (if enabled).
	var httpGateway *httpuigw.Gateway
	if cfg.Gateways.HTTP.Enabled {
		httpGateway = httpuigw.New(
			cfg.SyncDir,
			cfg.Ignore,
			httpuigw.Config{
				ListenAddr:     cfg.Gateways.HTTP.ListenAddr,
				Username:       cfg.Gateways.HTTP.Username,
				Password:       cfg.Gateways.HTTP.Password,
				MaxUploadBytes: cfg.MaxUploadBytes,
			},
			logger,
		)
		components = append(components, component{"HTTP file server", httpGateway.Start})
	}

	// SFTP Gateway (if enabled).
	var sftpGateway *sftpgw.Gateway
	if cfg.Gateways.SFTP.Enabled {
		sftpGateway, err = sftpgw.New(
			cfg.SyncDir,
			cfg.Ignore,
			cfg.MetaDir,
			sftpgw.Config{
				ListenAddr:     cfg.Gateways.SFTP.ListenAddr,
				Username:       cfg.Gateways.SFTP.Username,
				Password:       cfg.Gateways.SFTP.Password,
				HostKeyPath:    cfg.Gateways.SFTP.HostKeyPath,
				MaxUploadBytes: cfg.MaxUploadBytes,
			},
			logger,
		)
		if err != nil {
			return fmt.Errorf("sftp gateway init: %w", err)
		}
		components = append(components, component{"SFTP gateway", sftpGateway.Start})
	}

	var wg sync.WaitGroup
	componentErrors := make(chan error, 1)
	for _, part := range components {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := part.run(ctx); err != nil {
				err = fmt.Errorf("%s: %w", part.name, err)
				logger.Error("component failed", "error", err)
				select {
				case componentErrors <- err:
				default:
				}
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

	// Every listener drains at once within one budget. One after another they
	// could each wait out the whole budget, past the point where the container
	// runtime kills the process.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownDrain)
	defer shutdownCancel()
	stops := map[string]func(context.Context) error{"HTTP server": httpServer.Shutdown}
	if s3Gateway != nil {
		stops["S3 gateway"] = s3Gateway.Stop
	}
	if webdavGateway != nil {
		stops["WebDAV gateway"] = webdavGateway.Stop
	}
	if httpGateway != nil {
		stops["HTTP file server"] = httpGateway.Stop
	}
	var stopping sync.WaitGroup
	for name, stop := range stops {
		stopping.Add(1)
		go func() {
			defer stopping.Done()
			if err := stop(shutdownCtx); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					logger.Warn(name+" closed transfers still open after the drain period", "drain", shutdownDrain)
				} else {
					logger.Error(name+" shutdown error", "error", err)
				}
			}
		}()
	}
	stopping.Wait()

	// Cancellation closes the SFTP accept loop. Join it before Stop reads its
	// listener and waits for connections, including when another component failed
	// before SFTP finished starting.
	wg.Wait()

	// Graceful SFTP gateway shutdown.
	if sftpGateway != nil {
		if err := sftpGateway.Stop(shutdownCtx); err != nil {
			logger.Error("SFTP gateway shutdown error", "error", err)
		}
	}

	logger.Info("birak daemon stopped")
	if dropped := logs.Dropped(); dropped > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d log records were dropped; the log consumer could not keep up\n", dropped)
	}
	select {
	case err := <-componentErrors:
		return err
	default:
		return nil
	}
}

// loadPeerRoots adds a PEM bundle to the system roots for verifying peers.
func loadPeerRoots(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read peer_ca_file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("peer_ca_file %s contains no PEM certificates", path)
	}
	return roots, nil
}
