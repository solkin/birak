// birakctl provisions offline certificates and administers a quorum cluster.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"errors"
	"github.com/birak/birak/internal/cluster"
	"github.com/birak/birak/internal/generation"
	"github.com/birak/birak/internal/quorum"
	"github.com/hashicorp/raft"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: birakctl ca|issue|status|metrics|ready|join|catch-up|promote|remove|cancel|transfer|snapshot|scrub|collect|end-collection|backup|restore|upgrade-v2 [flags]")
	}
	op := args[0]
	f := flag.NewFlagSet(op, flag.ContinueOnError)
	backupFile := f.String("file", "", "backup archive path (new destination for backup)")
	id := f.String("cluster", "", "cluster ID")
	dir := f.String("dir", "", "offline CA directory")
	node := f.String("node", "", "target server identity")
	member := f.String("member", "", "member to add/promote/remove")
	address := f.String("address", "", "target HTTPS host:port")
	memberAddress := f.String("member-address", "", "new member HTTPS host:port")
	role := f.String("role", "node", "certificate role: node or admin")
	name := f.String("name", "", "certificate identity to issue")
	hosts := f.String("hosts", "", "comma-separated certificate DNS/IP names")
	ca := f.String("ca", "", "CA certificate file")
	cert := f.String("cert", "", "operator certificate file")
	key := f.String("key", "", "operator private key file")
	operator := f.String("operator", "", "operator certificate identity")
	timeout := f.Duration("timeout", 30*time.Minute, "operation deadline")
	grace := f.Duration("grace", 24*time.Hour, "minimum age of unreferenced bytes for collection")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if op == "restore" {
		if !cluster.ValidID(*id) || !cluster.ValidID(*node) {
			return fmt.Errorf("valid --cluster and --node required")
		}
		if *backupFile == "" || *timeout <= 0 {
			return fmt.Errorf("--file and positive --timeout required")
		}
		file, err := os.Open(*backupFile)
		if err != nil {
			return err
		}
		defer file.Close()
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		return quorum.RestoreS3(ctx, *dir, quorum.Identity{Cluster: *id, Node: raft.ServerID(*node), Format: quorum.Format}, raft.ServerAddress(*address), file)
	}
	if op == "upgrade-v2" {
		return quorum.UpgradeV2(*dir, quorum.Identity{Cluster: *id, Node: raft.ServerID(*node), Format: quorum.Format})
	}
	if op == "ca" {
		if *dir == "" {
			return fmt.Errorf("--dir is required")
		}
		return cluster.CreateCA(*dir, *id)
	}
	if op == "issue" {
		if *dir == "" {
			return fmt.Errorf("--dir is required")
		}
		return cluster.IssueCertificate(*dir, cluster.Principal{Cluster: *id, Role: *role, ID: *name}, strings.Split(*hosts, ","))
	}
	switch op {
	case "metrics", "backup", "collect", "end-collection", "scrub", "status", "ready", "join", "catch-up", "promote", "remove", "cancel", "transfer", "snapshot":
	default:
		return fmt.Errorf("unknown command %s", op)
	}
	if op == "backup" {
		if *backupFile == "" {
			return fmt.Errorf("--file is required")
		}
		if _, err := os.Lstat(*backupFile); !os.IsNotExist(err) {
			return fmt.Errorf("backup destination must not exist")
		}
	}
	if _, _, err := net.SplitHostPort(*address); err != nil {
		return fmt.Errorf("invalid --address: %w", err)
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	credentials, err := cluster.LoadCredentials(*ca, *cert, *key, cluster.Principal{Cluster: *id, Role: "admin", ID: *operator})
	if err != nil {
		return err
	}
	if !cluster.ValidID(*node) {
		return fmt.Errorf("--node is required")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: credentials.ClientTLS(*node)}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	method := "POST"
	var body io.Reader
	if op == "status" || op == "ready" || op == "backup" || op == "metrics" {
		method = "GET"
	} else {
		targetID := *member
		if op == "snapshot" || op == "collect" || op == "end-collection" || op == "scrub" {
			targetID = *node
		}
		b, _ := json.Marshal(cluster.Member{ID: targetID, Address: *memberAddress, Grace: *grace})
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+*address+"/v1/admin/"+op, body)
	if err != nil {
		return err
	}
	r, err := client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if op == "backup" && r.StatusCode == 200 {
		if *backupFile == "" {
			return fmt.Errorf("--file is required")
		}
		if _, err := os.Lstat(*backupFile); !os.IsNotExist(err) {
			return fmt.Errorf("backup destination must not exist")
		}
		file, err := os.CreateTemp(filepath.Dir(*backupFile), ".birak-backup-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		defer file.Close()
		if _, err = io.Copy(file, r.Body); err != nil {
			return err
		}
		if r.Trailer.Get("X-Birak-Backup-Complete") != "true" {
			return fmt.Errorf("backup stream incomplete")
		}
		if err = file.Sync(); err != nil {
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		// Link publishes exclusively; an existing archive can never be overwritten.
		if err = os.Link(file.Name(), *backupFile); err != nil {
			return err
		}
		return errors.Join(os.Remove(file.Name()), generation.SyncDir(filepath.Dir(*backupFile)))
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if r.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", r.StatusCode, b)
	}
	_, err = os.Stdout.Write(b)
	return err
}
