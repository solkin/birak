package quorum

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

const backupFormat = "birak-s3-backup-v1"
const maxBackupRecords = 10_000_000
const maxBackupRecordBytes = 128 << 10

type backupHeader struct {
	Format, Cluster string
	Index           uint64
	Records, Blobs  int
}
type backupSeal struct{ MetadataSHA256 string }

// ExportS3 writes one committed boundary of completed objects and buckets.
// In-flight multipart uploads and Raft identities/history are intentionally
// excluded. A terminal seal binds metadata; each blob has its own SHA-256.
func (n *Node) ExportS3(ctx context.Context, w io.Writer) error {
	if !n.exporting.CompareAndSwap(false, true) {
		return errors.New("backup already running")
	}
	defer n.exporting.Store(false)
	if err := n.lock(ctx); err != nil {
		return err
	}
	if err := n.barrier(ctx); err != nil {
		n.unlock()
		return err
	}
	n.fsm.mu.RLock()
	if n.fsm.state.Collection != nil {
		n.fsm.mu.RUnlock()
		n.unlock()
		return ErrMaintenance
	}
	var records []Record
	refs := map[string]generation.Ref{}
	index := n.fsm.state.Index
	for key, e := range n.fsm.state.Entries {
		if e.Deleted || (!strings.HasPrefix(key, "b/") && !strings.HasPrefix(key, "o/")) {
			continue
		}
		records = append(records, Record{key, e})
		if !e.MetaOnly {
			refs[e.Ref.Hash] = e.Ref
		}
	}
	n.fsm.mu.RUnlock()
	if len(records) > maxBackupRecords {
		n.unlock()
		return errors.New("backup record limit exceeded")
	}
	release := n.objects.PinReferences(refs)
	defer release()
	n.unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	hashes := make([]string, 0, len(refs))
	for hash := range refs {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	tw := tar.NewWriter(w)
	digest := sha256.New()
	writeJSON := func(name string, v any, seal bool) error {
		b, err := json.Marshal(v)
		if len(b) > maxBackupRecordBytes {
			return errors.New("backup record too large")
		}
		if err != nil {
			return err
		}
		if !seal {
			digest.Write(b)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	}
	if err := writeJSON("header.json", backupHeader{backupFormat, n.id.Cluster, index, len(records), len(refs)}, false); err != nil {
		return err
	}
	for i, rec := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeJSON(fmt.Sprintf("records/%d", i), rec, false); err != nil {
			return err
		}
	}
	for _, hash := range hashes {
		ref := refs[hash]
		f, err := n.openGeneration(ctx, ref)
		if err != nil {
			return err
		}
		err = tw.WriteHeader(&tar.Header{Name: "blobs/" + hash, Mode: 0600, Size: ref.Size, Typeflag: tar.TypeReg})
		if err == nil {
			h := sha256.New()
			_, err = io.CopyN(io.MultiWriter(tw, h), f, ref.Size)
			if err == nil && hex.EncodeToString(h.Sum(nil)) != hash {
				err = generation.ErrCorrupt
			}
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	if err := writeJSON("seal.json", backupSeal{hex.EncodeToString(digest.Sum(nil))}, true); err != nil {
		return err
	}
	return tw.Close()
}

// RestoreS3 creates a NEW standalone state directory and NEW cluster identity.
// Its in-memory transport is never reachable by clients. The destination is
// publishable only after every hash/seal and transaction succeeds and the node
// closes durably. Any failure leaves a persistent marker refusing daemon startup.
// Existing destinations are never overwritten. Restart with a fresh destination.
func RestoreS3(ctx context.Context, dir string, id Identity, address raft.ServerAddress, r io.Reader) (err error) {
	if _, _, err := net.SplitHostPort(string(address)); err != nil {
		return fmt.Errorf("restore requires the new advertised host:port: %w", err)
	}
	if dir == "" || id.Cluster == "" || id.Node == "" || id.Format != Format {
		return errors.New("explicit new directory, cluster and node required")
	}
	if _, e := os.Lstat(dir); !os.IsNotExist(e) {
		return errors.New("restore destination must not exist")
	}
	if err = generation.MakeDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err = generation.SyncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	marker := filepath.Join(dir, "restore-incomplete")
	// The marker is durable BEFORE node creation, including crash windows.
	if err = writeMarker(marker); err != nil {
		return err
	}
	// No network listener is opened; keep the exclusive lease through restore.
	_, transport := raft.NewInmemTransport(address)
	defer transport.Close()
	cfg := raft.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	cfg.ElectionTimeout = 100 * time.Millisecond
	cfg.LeaderLeaseTimeout = 50 * time.Millisecond
	cfg.CommitTimeout = time.Millisecond
	n, e := Open(Options{restoring: true, Dir: dir, Identity: id, Bootstrap: true, Transport: transport, Peers: offlinePeers{}, RaftConfig: cfg, Timeout: 30 * time.Second})
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, n.Close()) }()
	for n.barrier(ctx) != nil {
		if err = ctx.Err(); err != nil {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	tr := tar.NewReader(r)
	digest := sha256.New()
	readJSON := func(name string, v any, seal bool) error {
		h, err := tr.Next()
		if err != nil {
			return err
		}
		if h.Name != name || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxBackupRecordBytes {
			return errors.New("invalid backup record header")
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if !seal {
			digest.Write(b)
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err = d.Decode(v); err != nil {
			return err
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			return errors.New("trailing record data")
		}
		return nil
	}
	var header backupHeader
	if err = readJSON("header.json", &header, false); err != nil {
		return err
	}
	if header.Format != backupFormat || header.Cluster == id.Cluster || header.Records < 0 || header.Records > maxBackupRecords || header.Blobs < 0 || header.Blobs > header.Records {
		return errors.New("invalid backup header or reused cluster identity")
	}
	records := make([]Record, 0, min(header.Records, 10000))
	refs := map[string]generation.Ref{}
	buckets := map[string]bool{}
	last := ""
	for i := 0; i < header.Records; i++ {
		var rec Record
		if err = readJSON(fmt.Sprintf("records/%d", i), &rec, false); err != nil {
			return err
		}
		c := Change{Key: rec.Key, Ref: rec.Entry.Ref, MetaOnly: rec.Entry.MetaOnly, Attributes: rec.Entry.Attributes}
		if rec.Key <= last || rec.Entry.Deleted || rec.Entry.Index == 0 || rec.Entry.Index > header.Index {
			return errors.New("invalid backup record order or index")
		}
		last = rec.Key
		if err = (Mutation{ID: "restore", Changes: []Change{c}}).validate(); err != nil {
			return err
		}
		if strings.HasPrefix(rec.Key, "b/") && rec.Entry.MetaOnly && len(strings.Split(rec.Key, "/")) == 2 && len(rec.Key) > 2 {
			buckets[strings.TrimPrefix(rec.Key, "b/")] = true
		} else if strings.HasPrefix(rec.Key, "o/") && !rec.Entry.MetaOnly {
			parts := strings.SplitN(rec.Key, "/", 3)
			if len(parts) != 3 || parts[2] == "" || !buckets[parts[1]] {
				return errors.New("object without bucket")
			}
			if ref, ok := refs[rec.Entry.Ref.Hash]; ok && ref != rec.Entry.Ref {
				return errors.New("conflicting blob sizes")
			}
			refs[rec.Entry.Ref.Hash] = rec.Entry.Ref
		} else {
			return errors.New("backup includes unsupported namespace")
		}
		records = append(records, rec)
	}
	if len(refs) != header.Blobs {
		return errors.New("incorrect backup blob count")
	}
	hashes := make([]string, 0, len(refs))
	for hash := range refs {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	for _, hash := range hashes {
		if err = ctx.Err(); err != nil {
			return err
		}
		h, e := tr.Next()
		if e != nil {
			return e
		}
		ref := refs[hash]
		if h.Name != "blobs/"+hash || h.Typeflag != tar.TypeReg || h.Size != ref.Size {
			return errors.New("invalid backup blob header")
		}
		if err = n.ReceiveBlob(ctx, ref, tr); err != nil {
			return err
		}
	}
	var seal backupSeal
	if err = readJSON("seal.json", &seal, true); err != nil {
		return err
	}
	if seal.MetadataSHA256 != hex.EncodeToString(digest.Sum(nil)) {
		return errors.New("backup metadata checksum mismatch")
	}
	if _, e = tr.Next(); e != io.EOF {
		return errors.New("trailing backup entries")
	}
	// Require end of the transport too: no concatenated or hidden second archive.
	var extra [1]byte
	if count, e := r.Read(extra[:]); count != 0 || e != io.EOF {
		return errors.New("trailing backup bytes")
	}
	for _, rec := range records {
		// Bound each command independently of object-key/attribute sizes. Restoration
		// is offline; no request can observe this progressive reconstruction.
		if _, err = n.TransactOnce(ctx, "restore", []Change{{Key: rec.Key, Ref: rec.Entry.Ref, MetaOnly: rec.Entry.MetaOnly, Attributes: rec.Entry.Attributes}}, nil); err != nil {
			return err
		}
	}
	if len(records) > 0 {
		if err = n.Snapshot(ctx); err != nil {
			return err
		}
	}
	if err = n.Close(); err != nil {
		return err
	}
	if err = os.Remove(marker); err != nil {
		return err
	}
	return generation.SyncDir(dir)
}
func writeMarker(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString("offline restore incomplete\n")
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return generation.SyncDir(filepath.Dir(path))
}

type offlinePeers struct{}

func (offlinePeers) Identity(context.Context, raft.ServerID) (Identity, error) {
	return Identity{}, errors.New("offline")
}
func (offlinePeers) Receive(context.Context, raft.ServerID, generation.Ref, io.Reader) error {
	return errors.New("offline")
}
func (offlinePeers) Open(context.Context, raft.ServerID, generation.Ref) (io.ReadCloser, error) {
	return nil, errors.New("offline")
}
