package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Set EREGION_TEST_AWS_DIR to an explicitly provisioned CLI directory to
// exercise the real CLI against an in-process fake S3 server, with dummy keys.
func TestPinnedAWSCLIConditionalHTTP(t *testing.T) {
	dir := os.Getenv("EREGION_TEST_AWS_DIR")
	if dir == "" {
		t.Skip("pinned AWS CLI directory not supplied")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_PAGER", "")
	var mu sync.Mutex
	var conditions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"current"`)
			_, _ = io.WriteString(w, "pointer")
			return
		}
		mu.Lock()
		conditions = append(conditions, r.Header.Get("If-Match")+"|"+r.Header.Get("If-None-Match"))
		count := len(conditions)
		mu.Unlock()
		if count == 3 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "manifest")
	if err := os.WriteFile(file, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &productionStore{bucket: "test", key: "edge/manifest.json", endpoint: server.URL, manifest: file}
	if etag, exists, err := s.get(context.Background(), s.key, file+".read"); err != nil || !exists || etag != `"current"` {
		t.Fatalf("GET ETag: %q %v %v", etag, exists, err)
	}
	if err := s.write(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.write(context.Background(), `"current"`); err != nil {
		t.Fatal(err)
	}
	if err := s.write(context.Background(), `"current"`); err != errConflict {
		t.Fatalf("412: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(conditions) != 3 || conditions[0] != "|*" || conditions[1] != `"current"|` || conditions[2] != `"current"|` {
		t.Fatalf("conditions: %q", conditions)
	}
}
