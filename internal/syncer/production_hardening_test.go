package syncer

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func TestExtendedClockReplicatesOverHTTPAfterLocalPublication(t *testing.T) {
	source, _ := auditSyncer(t)
	destination, _ := auditSyncer(t)
	meta := auditMeta("file", "original", 1)
	meta.Clock, meta.BigClock = math.MaxInt64, "9999999999999999999999999999999999999999"
	auditIndex(t, source, meta, "original")
	if _, err := source.store.PutRemote(meta); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	if err := destination.applyChange(context.Background(), peer.URL, meta); err != nil {
		t.Fatal(err)
	}
	stage, err := fileops.CreateTemp(destination.syncDir, ".birak-tmp-clock-*")
	if err != nil {
		t.Fatal(err)
	}
	defer fileops.ReleaseTemp(stage)
	if _, err := stage.WriteString("new local publication"); err != nil {
		t.Fatal(err)
	}
	if err := fileops.Publish(destination.syncDir, stage.Name(), filepath.Join(destination.syncDir, "file")); err != nil {
		t.Fatal(err)
	}
	updated, err := destination.store.GetFile("file")
	if err != nil {
		t.Fatal(err)
	}
	if store.CompareClock(*updated, meta) <= 0 {
		t.Fatalf("local publication lost extended clock: %+v", updated)
	}
	back := httptest.NewServer(server.New(destination.store, destination.syncDir, "destination", nil, server.Config{}, destination.logger).Handler())
	defer back.Close()
	if err := source.applyChange(context.Background(), back.URL, *updated); err != nil {
		t.Fatal(err)
	}
	got, err := source.store.GetFile("file")
	if err != nil || store.CompareState(got, updated) != 0 {
		t.Fatalf("HTTP replica lost extended state: %+v %v", got, err)
	}
	body, err := os.ReadFile(filepath.Join(source.syncDir, "file"))
	if err != nil || string(body) != "new local publication" {
		t.Fatalf("wrong replica bytes: %q %v", body, err)
	}
}

func TestReplicaCaseCollisionCannotReplaceBytesOrLoseRepair(t *testing.T) {
	s, _ := auditSyncer(t)
	first := auditMeta("bucket/App.apk", "original", time.Now().UnixNano())
	auditIndex(t, s, first, "original")
	second := auditMeta("bucket/app.apk", "replacement", first.ModTime+1)
	peer := "http://peer"
	if err := s.store.EnqueueChange(peer, second, "case collision"); err != nil {
		t.Fatal(err)
	}
	if err := s.applyChange(context.Background(), peer, second); !errors.Is(err, store.ErrNameCollision) {
		t.Fatalf("accepted alias: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.syncDir, first.Name))
	if err != nil || string(got) != "original" {
		t.Fatalf("original replaced: %q %v", got, err)
	}
	pending, err := s.store.PendingRepairCount(peer)
	if err != nil || pending != 1 {
		t.Fatalf("collision lost repair: %d %v", pending, err)
	}
}

func TestInitialAdmissionRequiresAppliedSourceAndSurvivesOutageAndRestart(t *testing.T) {
	s, db := auditSyncer(t)
	s.peers = []string{"http://source", "http://offline"}
	s.initialized.Store(false)
	if err := s.store.SetNodeValue("replica_initialized", "0"); err != nil {
		t.Fatal(err)
	}
	meta := auditMeta("file", "body", 1)
	if err := s.store.EnqueueChange(s.peers[0], meta, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	s.withStat(s.peers[0], func(st *peerStat) {
		st.lastSuccess = time.Now()
		st.lastReconcile = time.Now()
		st.epoch = "source"
		st.peerMaxVersion = 1
		st.cursor = 1
	})
	if s.ServingReady() {
		t.Fatal("received cursor admitted unapplied replica")
	}
	if _, err := s.store.PutRemote(meta); err != nil {
		t.Fatal(err)
	}
	// A restart with partial data must not infer admission from a positive version.
	s.initializeAdmission()
	if s.ServingReady() {
		t.Fatal("partial restart bypassed the bootstrap marker")
	}
	if err := s.store.ResolveRepair(s.peers[0], meta.Name); err != nil {
		t.Fatal(err)
	}
	if !s.ServingReady() {
		t.Fatal("one fully applied source did not admit replica")
	}
	s.withStat(s.peers[0], func(st *peerStat) { st.consecutiveErrs = 100; st.lastSuccess = time.Time{} })
	if !s.ServingReady() {
		t.Fatal("outage revoked independent replica")
	}
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openNamespaceNode(t, s.syncDir, db, s.nodeID)
	reopened.peers = s.peers
	reopened.initializeAdmission()
	if !reopened.ServingReady() {
		t.Fatal("restart lost durable admission")
	}
}
