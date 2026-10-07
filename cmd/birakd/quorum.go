package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/birak/birak/internal/cluster"
	"github.com/birak/birak/internal/config"
	s3gw "github.com/birak/birak/internal/gateway/s3"
	"github.com/birak/birak/internal/multipart"
	"github.com/birak/birak/internal/quorum"
	"github.com/hashicorp/raft"
)

func runQuorum(cfg config.Config, bootstrap bool, logger *slog.Logger) error {
	credentials, err := cluster.LoadCredentials(cfg.Quorum.CAFile, cfg.Quorum.CertFile, cfg.Quorum.KeyFile, cluster.Principal{Cluster: cfg.Quorum.ClusterID, Role: "node", ID: cfg.NodeID})
	if err != nil {
		return err
	}
	var seeds []cluster.Member
	for _, m := range cfg.Quorum.Seeds {
		seeds = append(seeds, cluster.Member{ID: m.ID, Address: m.Address})
	}
	raftConfig := raft.DefaultConfig()
	raftConfig.LogLevel = cfg.LogLevel
	s, err := cluster.Open(cluster.Options{
		Dir: cfg.Quorum.StateDir, ID: cfg.NodeID, Cluster: cfg.Quorum.ClusterID,
		Listen: cfg.Quorum.ListenAddr, Advertise: cfg.Quorum.AdvertiseAddr,
		Credentials: credentials, Seeds: seeds, Bootstrap: bootstrap,
		MaxBlobBytes: cfg.MaxUploadBytes, OperationTimeout: cfg.Quorum.OperationTimeout,
		TransferTimeout: cfg.Quorum.TransferTimeout, RaftConfig: raftConfig,
	})
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	g := s3gw.New("", nil, s3gw.Config{
		ListenAddr: cfg.Gateways.S3.ListenAddr,
		AccessKey:  cfg.Gateways.S3.AccessKey, SecretKey: cfg.Gateways.S3.SecretKey,
		Domain: cfg.Gateways.S3.Domain, MaxUploadBytes: cfg.MaxUploadBytes,
		Quorum: s.Node(), TransferTimeout: cfg.Quorum.TransferTimeout, Forward: s.ForwardS3,
		TLSCertFile: cfg.Gateways.S3.TLSCertFile, TLSKeyFile: cfg.Gateways.S3.TLSKeyFile,
		QuorumLimits: multipart.Limits{
			MinPartBytes: cfg.Multipart.MinPartBytes, MaxPartBytes: cfg.Multipart.MaxPartBytes,
			MaxParts: cfg.Multipart.MaxParts, MaxActiveUploads: cfg.Multipart.MaxActiveUploads,
			MaxConcurrentParts: cfg.Multipart.MaxConcurrentPartUploads,
		},
	}, logger)
	s.SetS3(g.Handler())
	defer func() {
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		g.Stop(shutdown)
		s.Close(shutdown)
	}()
	logger.Info("quorum node started", "address", s.Address(), "cluster", cfg.Quorum.ClusterID, "bootstrap", bootstrap)
	failed := make(chan error, 1)
	if cfg.Gateways.S3.Enabled {
		go func() { failed <- g.Start(ctx) }()
	}
	// Configured buckets are created through consensus after an election, never
	// by creating local directories or acknowledging an uncommitted bootstrap.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		pending := append([]string(nil), cfg.Gateways.S3.Buckets...)
		for len(pending) > 0 {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.Node().Status().State != "Leader" {
					continue
				}
				for len(pending) > 0 {
					bucket := pending[0]
					_, exists, e := s.Node().Lookup(ctx, "b/"+bucket)
					if e != nil {
						break
					}
					if !exists {
						_, e = s.Node().Transact(ctx, "configured-bucket:"+bucket, []quorum.Change{{Key: "b/" + bucket, MetaOnly: true}}, []quorum.Condition{{Key: "b/" + bucket}})
						if e != nil {
							break
						}
					}
					pending = pending[1:]
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.Done():
		return err
	case err := <-failed:
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}
