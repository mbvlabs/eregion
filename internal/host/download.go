package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// BuildpackVersion is the pinned Cloud Native Buildpacks pack CLI release.
const BuildpackVersion = "0.40.9"

type artifact struct {
	URL    string
	SHA256 string
}

var packArtifacts = map[string]artifact{
	"amd64": {
		URL:    "https://github.com/buildpacks/pack/releases/download/v" + BuildpackVersion + "/pack-v" + BuildpackVersion + "-linux.tgz",
		SHA256: "dc0ee1e931cf8a106d7555a01a214864f9acb60b77adf15d69b74df4404758e9",
	},
	"arm64": {
		URL:    "https://github.com/buildpacks/pack/releases/download/v" + BuildpackVersion + "/pack-v" + BuildpackVersion + "-linux-arm64.tgz",
		SHA256: "091ccb213823656c727731537ef8f1000eb4dc3ec61641506653e7f9d6da0c5e",
	},
}

// PackArtifact returns the pinned archive for an architecture. The checksum is
// part of the plan and is verified before any byte is installed.
func PackArtifact(arch string) (version, downloadURL, checksum string, ok bool) {
	if arch == "" {
		arch = runtime.GOARCH
	}
	entry, ok := packArtifacts[arch]
	if !ok {
		return "", "", "", false
	}
	return BuildpackVersion, entry.URL, entry.SHA256, true
}

// Downloader is the test seam for pinned artifact retrieval. No curl or shell
// pipeline is involved: bootstrap downloads bytes and verifies the digest in
// Go before extracting a single member.
type Downloader interface {
	Get(context.Context, string, int64, string) ([]byte, error)
}

// HTTPDownloader fetches HTTPS URLs only. It rejects redirects to another
// scheme or host and caps the body before reading it into memory.
type HTTPDownloader struct {
	Client *http.Client
}

func (d HTTPDownloader) Get(ctx context.Context, rawURL string, max int64, wantSHA string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("artifact URL must be an absolute HTTPS URL")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	if d.Client != nil {
		copy := *d.Client
		client = &copy
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		// GitHub release downloads redirect to its dedicated asset origin.
		allowed := req.URL.Host == parsed.Host || (parsed.Host == "github.com" && req.URL.Host == "release-assets.githubusercontent.com")
		if req.URL.Scheme != "https" || !allowed || req.URL.User != nil {
			return fmt.Errorf("artifact redirect changed the trusted HTTPS origin")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download the pinned artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download the pinned artifact: HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("read the pinned artifact: %w", err)
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("the pinned artifact is larger than the %d byte limit", max)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantSHA) {
		return nil, fmt.Errorf("pinned artifact checksum mismatch: got %s", got)
	}
	return raw, nil
}

// extractPack reads the single pack executable from the pinned tar.gz. It does
// not use tar's filesystem extraction path, so archive traversal is irrelevant.
func extractPack(archive []byte, max int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the pinned pack archive is not gzip: %w", err)
	}
	defer reader.Close()
	tarball := tar.NewReader(reader)
	var found []byte
	for {
		header, err := tarball.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the pinned pack archive: %w", err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		if name != "pack" {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("the pinned pack archive member is not a regular file")
		}
		if header.Size < 1 || header.Size > max {
			return nil, fmt.Errorf("the pinned pack binary has an unexpected size")
		}
		found, err = io.ReadAll(io.LimitReader(tarball, max+1))
		if err != nil {
			return nil, fmt.Errorf("read the pinned pack binary: %w", err)
		}
		if int64(len(found)) > max {
			return nil, fmt.Errorf("the pinned pack binary exceeds the size limit")
		}
		break
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("the pinned pack archive does not contain a pack binary")
	}
	return found, nil
}
