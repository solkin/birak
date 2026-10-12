package s3

import (
	"net/http"
	"strings"
)

// Streaming payloads need a separate aws-chunked decoder and integrity checks.
// Reject them before any handler creates state or replaces an object.
func unsupportedStreamingBody(r *http.Request) bool {
	for _, value := range r.Header.Values("Content-Encoding") {
		for _, encoding := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(encoding), "aws-chunked") {
				return true
			}
		}
	}
	for _, value := range r.Header.Values("X-Amz-Content-Sha256") {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(value)), "STREAMING-") {
			return true
		}
	}
	return false
}
