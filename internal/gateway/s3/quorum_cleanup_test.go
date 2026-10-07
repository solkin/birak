package s3

import (
	"context"
	"encoding/xml"
	"testing"
	"time"
)

func TestQuorumExpiryPreservesActivePartsAndExpiresCompletionSafely(t *testing.T) {
	g := quorumGateway(t)
	ctx := context.Background()
	qRequest(t, g, "PUT", "/bucket", "", nil, 200)
	start := func(key string) string {
		w := qRequest(t, g, "POST", "/bucket/"+key+"?uploads", "", nil, 200)
		var up InitiateMultipartUploadResult
		if err := xml.Unmarshal(w.Body.Bytes(), &up); err != nil {
			t.Fatal(err)
		}
		return up.UploadID
	}
	active := start("active")
	closed := start("closed")
	activePath := "/bucket/active?uploadId=" + active
	qRequest(t, g, "PUT", activePath+"&partNumber=1", "active", nil, 200)
	path := "/bucket/closed?uploadId=" + closed
	tag := qRequest(t, g, "PUT", path+"&partNumber=1", "old", nil, 200).Header().Get("ETag")
	payload := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>" + tag + "</ETag></Part></CompleteMultipartUpload>"
	qRequest(t, g, "POST", path, payload, nil, 200)
	qRequest(t, g, "PUT", "/bucket/closed", "new", nil, 200)
	if err := g.CleanupQuorum(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	records, err := g.config.Quorum.View(ctx, qParts(closed))
	if err != nil || len(records) != 0 {
		t.Fatal("closed parts retained", records, err)
	}
	records, err = g.config.Quorum.View(ctx, qParts(active))
	if err != nil || len(records) != 1 {
		t.Fatal("active part lost", records, err)
	}
	qRequest(t, g, "POST", path, payload, nil, 200)
	if got := qRequest(t, g, "GET", "/bucket/closed", "", nil, 200).Body.String(); got != "new" {
		t.Fatal("retry resurrected old bytes", got)
	}
	if err := g.CleanupQuorum(ctx, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	qRequest(t, g, "POST", path, payload, nil, 404)
	qRequest(t, g, "PUT", activePath+"&partNumber=1", "late", nil, 404)
	records, err = g.config.Quorum.View(ctx, "u/", "p/", "c/")
	if err != nil || len(records) != 0 {
		t.Fatal("expired metadata retained", records, err)
	}
	if got := qRequest(t, g, "GET", "/bucket/closed", "", nil, 200).Body.String(); got != "new" {
		t.Fatal(got)
	}
}
