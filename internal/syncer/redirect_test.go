package syncer

import (
	"context"
	"github.com/birak/birak/internal/server"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Metadata and file requests must never disclose credentials through redirects.
// Both endpoints are isolated test servers; the key is synthetic.
func TestSecretDoesNotFollowCrossHostRedirect(t *testing.T) {
	for _, download := range []bool{false, true} {
		name := "metadata"
		if download {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := auditSyncer(t)
			s.opts.Secret = "synthetic-production-review-secret"
			captured := make(chan string, 1)
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Get(server.HeaderSecret)
				w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
				_, _ = io.WriteString(w, "[]")
			}))
			defer other.Close()
			redirectURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1) + "/capture"
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, redirectURL, http.StatusFound)
			}))
			defer peer.Close()
			if download {
				_ = s.downloadAndApply(context.Background(), peer.URL, auditMeta("file", "payload", 1))
			} else {
				resp, _ := s.doRequest(context.Background(), peer.URL+"/changes?since=0")
				if resp != nil {
					resp.Body.Close()
				}
			}
			select {
			case got := <-captured:
				if got != "" {
					t.Fatalf("cluster secret disclosed to another host by %s request", name)
				}
			default:
			}
		})
	}
}
