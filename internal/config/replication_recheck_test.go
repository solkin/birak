package config

import "testing"

func TestReviewDuplicatePeerMustNotStartDuplicateWorkers(t *testing.T) {
	c := DefaultConfig()
	c.Peers = []string{"http://node-b:9100", "http://node-b:9100"}
	if err := c.validate(); err == nil && len(c.Peers) != 1 {
		t.Fatal("duplicate peer URL accepted without normalization; Run starts duplicate poll/repair/reconcile workers")
	}
}

func TestPeerNormalizationMatchesPersistentQueueKeys(t *testing.T) {
	c := DefaultConfig()
	c.Peers = []string{" http://peer:9000/ "}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Peers) != 1 || c.Peers[0] != "http://peer:9000" {
		t.Fatalf("startup pruning would remove active queue keys: %q", c.Peers)
	}
	c.Peers = []string{"http://peer:9000", "http://peer:9000/"}
	if err := c.validate(); err == nil {
		t.Fatal("normalized duplicate peer accepted")
	}
}
