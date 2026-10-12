package s3

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/birak/birak/internal/multipart"
)

type observedBody struct{ reads int }

func (b *observedBody) Read(p []byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func TestStreamingUploadsRejectBeforeReadingAndPreserveData(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
	}{
		{"encoding", http.Header{"Content-Encoding": {"aws-chunked"}}},
		{"mixed_encoding", http.Header{"Content-Encoding": {"gzip, AWS-CHUNKED"}}},
		{"repeated_encoding", http.Header{"Content-Encoding": {"gzip", "aws-chunked"}}},
		{"signed_marker", http.Header{"X-Amz-Content-Sha256": {"STREAMING-AWS4-HMAC-SHA256-PAYLOAD"}}},
		{"unsigned_trailer", http.Header{"X-Amz-Content-Sha256": {"STREAMING-UNSIGNED-PAYLOAD-TRAILER"}}},
		{"signed_trailer", http.Header{"X-Amz-Content-Sha256": {"STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, root, store := multipartGateway(t, Config{}, multipart.Limits{})
			object := filepath.Join(root, "bucket", "object")
			if err := os.WriteFile(object, []byte("previous object"), 0600); err != nil {
				t.Fatal(err)
			}
			id := initiateUpload(t, g, "bucket", "object")
			oldETag := uploadPart(t, g, "bucket", "object", id, 1, []byte("previous part"))
			g.config.AccessKey, g.config.SecretKey = "access", "secret"
			paths := []string{"/bucket/object", fmt.Sprintf("/bucket/object?uploadId=%s&partNumber=1", id)}
			for _, path := range paths {
				body := &observedBody{}
				req := httptest.NewRequest(http.MethodPut, path, body)
				req.Header = tc.headers.Clone()
				signV4Header(req, "access", "secret")
				w := httptest.NewRecorder()
				g.route(w, req)
				if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "<Code>NotImplemented</Code>") {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				if body.reads != 0 {
					t.Fatal("unsupported body was consumed")
				}
			}
			got, err := os.ReadFile(object)
			if err != nil || string(got) != "previous object" {
				t.Fatalf("object replaced: %q %v", got, err)
			}
			parts, _, _, err := store.ListParts(id, 0, 1000)
			if err != nil || len(parts) != 1 || `"`+parts[0].ETag+`"` != oldETag {
				t.Fatalf("staged part replaced: %v %v", parts, err)
			}
		})
	}
}
