package main

import (
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/cluster"
)

func TestOfflineCredentialCommands(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"ca", "--dir", dir, "--cluster", "test"}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "node"} {
		args := []string{"issue", "--dir", dir, "--cluster", "test", "--role", role, "--name", "identity"}
		if err := run(args); err != nil {
			t.Fatal(err)
		}
		if _, err := cluster.LoadCredentials(filepath.Join(dir, "ca.crt"), filepath.Join(dir, role+"-identity.crt"), filepath.Join(dir, role+"-identity.key"), cluster.Principal{Cluster: "test", Role: role, ID: "identity"}); err != nil {
			t.Fatal(err)
		}
		if err := run(args); err == nil {
			t.Fatal("overwrote credentials")
		}
	}
	for _, args := range [][]string{nil, {"issue"}, {"unknown"}, {"ca", "--dir", dir, "--cluster", "test"}, {"issue", "--dir", dir, "--cluster", "foreign", "--name", "new"}, {"status", "--address", "not-an-address"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
