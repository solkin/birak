package s3

import (
	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/multipart"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParity_CrossBucketSymlinkRead(t *testing.T) {
	g, root := testGateway(t, Config{})
	for _, bucket := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(root, bucket), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "second", "secret"), []byte("other bucket data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "second", "secret"), filepath.Join(root, "first", "alias")); err != nil {
		t.Fatal(err)
	}
	w := serveRequest(g, http.MethodGet, "/first/alias", nil, nil)
	if w.Code == 200 {
		t.Fatalf("cross-bucket alias read succeeded: %s", w.Body.String())
	}
}
func TestParity_DeleteBucketUnavailableStaging(t *testing.T) {
	g, root := testGateway(t, Config{})
	if err := os.Mkdir(filepath.Join(root, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}
	store, err := multipart.New(root, multipart.Limits{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	g.multipart = store
	if _, err := store.Create("bucket", "object"); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(root, gateway.ReservedDirName, "multipart")
	if err := os.Rename(staging, staging+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	w := serveRequest(g, http.MethodDelete, "/bucket", nil, nil)
	if w.Code != 500 {
		t.Fatalf("delete with unavailable staging: HTTP %d (want 500)", w.Code)
	}
}

func TestMultipart_RootPaths(t *testing.T) {
	for _, kind := range []string{"absolute", "relative", "relative-symlink"} {
		t.Run(kind, func(t *testing.T) {
			t.Chdir(t.TempDir())
			root := "files"
			if err := os.MkdirAll(filepath.Join(root, "bucket"), 0755); err != nil {
				t.Fatal(err)
			}
			if kind == "absolute" {
				var err error
				root, err = filepath.Abs(root)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "relative-symlink" {
				if err := os.Symlink("files", "alias"); err != nil {
					t.Fatal(err)
				}
				root = "alias"
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			store, err := multipart.New(root, multipart.Limits{}, logger)
			if err != nil {
				t.Fatal(err)
			}
			g := New(root, nil, Config{Multipart: store}, logger)
			id := initiateUpload(t, g, "bucket", "object")
			etag := uploadPart(t, g, "bucket", "object", id, 1, []byte("payload"))
			w := completeUpload(g, "bucket", "object", id, []CompletePartEntry{{PartNumber: 1, ETag: etag}})
			if w.Code != http.StatusOK {
				t.Fatalf("complete: %d: %s", w.Code, w.Body.String())
			}
			got, err := os.ReadFile(filepath.Join(root, "bucket", "object"))
			if err != nil || string(got) != "payload" {
				t.Fatalf("published object: %q %v", got, err)
			}
			if strings.Contains(w.Body.String(), root+"/bucket") {
				t.Fatal("response leaked filesystem root")
			}
		})
	}
}
