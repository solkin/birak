package store

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestExtendedClockSurvivesRestartNamespaceAndWire(t *testing.T) {
	s := newTestStore(t)
	seed := FileMeta{Name: "tree/child", Clock: math.MaxInt64, BigClock: strings.Repeat("9", 80), Hash: "old", ModTime: 1}
	if _, err := s.PutRemote(seed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutLocal(FileMeta{Name: "tree", Hash: "replacement", ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	parent, err := s.GetFile("tree")
	if err != nil {
		t.Fatal(err)
	}
	if parent.BigClock != "1"+strings.Repeat("0", 80) || CompareClock(*parent, seed) <= 0 {
		t.Fatalf("carry/order: %+v", parent)
	}
	for _, meta := range []FileMeta{*parent, Superseded("tree/child", *parent), ConflictCopy(*parent)} {
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		var decoded FileMeta
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := ValidateClock(decoded); err != nil {
			t.Fatal(err)
		}
		if CompareState(&decoded, &meta) != 0 {
			t.Fatalf("wire changed rank: %+v", decoded)
		}
	}
	if _, err := s.PutRemote(Superseded("tree/child", *parent)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutLocal(FileMeta{Name: "tree/child", ModTime: 1, Hash: "recreated"}); err != nil {
		t.Fatal(err)
	}
	child, err := s.GetFile("tree/child")
	if err != nil {
		t.Fatal(err)
	}
	if CompareClock(*child, *parent) <= 0 {
		t.Fatal("child did not advance beyond extended parent")
	}
	// Reopening the same DB must reconstruct the extended clock, not its int64 projection.
	var path string
	if err := s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(path, s.logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.PutLocal(FileMeta{Name: "tree/child", ModTime: 1, Hash: "again"}); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetFile("tree/child")
	if err != nil {
		t.Fatal(err)
	}
	if CompareClock(*got, *child) <= 0 {
		t.Fatal("restart lost extended rank")
	}
}

func TestMalformedExtendedClocksRejectedWithoutChangingState(t *testing.T) {
	s := newTestStore(t)
	for _, clock := range []string{"0", "01", "9223372036854775807", "-9223372036854775808", "9223372036854775808x"} {
		if _, err := s.PutRemote(FileMeta{Name: "file", Clock: math.MaxInt64, BigClock: clock}); err == nil {
			t.Fatalf("accepted %q", clock)
		}
	}
	got, err := s.GetFile("file")
	if err != nil || got != nil {
		t.Fatalf("invalid clock published state: %+v %v", got, err)
	}
}

func TestPortableNamesProtectDirectoriesUnicodeAndDeletionHistory(t *testing.T) {
	for _, pair := range [][2]string{{"bucket/App.apk", "bucket/app.apk"}, {"Bucket/a", "bucket/b"}, {"bucket/caf\u00e9", "bucket/cafe\u0301"}, {"bucket/Stra\u00dfe", "bucket/STRASSE"}} {
		s := newTestStore(t)
		if _, err := s.PutLocal(FileMeta{Name: pair[0], Hash: "original", ModTime: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.CheckName(pair[1]); !errors.Is(err, ErrNameCollision) {
			t.Fatalf("collision %q: %v", pair, err)
		}
		if _, err := s.PutLocal(FileMeta{Name: pair[0], Deleted: true, ModTime: 2}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutRemote(FileMeta{Name: pair[1], Hash: "other", Clock: 3}); !errors.Is(err, ErrNameCollision) {
			t.Fatalf("deletion forgot spelling: %v", err)
		}
	}
}

func TestPortableNameMigrationRejectsAmbiguousLegacyMetadata(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PutLocal(FileMeta{Name: "App.apk", Hash: "one", ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO files(name,mod_time,size,hash,deleted,version) VALUES ('app.apk',2,0,'two',0,2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM node_meta WHERE key='portable_names_v1'"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateNames(); !errors.Is(err, ErrNameCollision) {
		t.Fatalf("migration accepted ambiguous names: %v", err)
	}
	var marker int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM node_meta WHERE key='portable_names_v1'").Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("failed migration committed: %d %v", marker, err)
	}
	got, err := s.GetFile("App.apk")
	if err != nil || got.Hash != "one" {
		t.Fatalf("migration altered original: %+v %v", got, err)
	}
}
