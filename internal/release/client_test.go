package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type identityVerifier struct{ want string }

func (v identityVerifier) VerifyBlob(_ context.Context, _, bundle []byte, identity string) error {
	if identity != v.want || string(bundle) != "signed-by:"+identity {
		return fmt.Errorf("wrong signer or bundle")
	}
	return nil
}

func TestPinnedManifestUsesExactTagIdentity(t *testing.T) {
	m := sampleManifest(t)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := Identity(ChannelStable, m.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, signer := range []string{identity, strings.Replace(identity, "alpha.2", "alpha.1", 1), strings.Replace(identity, "publish-stable.yml", "publish-edge.yml", 1)} {
		client := &Client{Verifier: identityVerifier{want: identity}, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(r.URL.Path, "/releases/"+m.Version+"/") {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			body := string(raw)
			if strings.HasSuffix(r.URL.Path, ".sigstore.json") {
				body = "signed-by:" + signer
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}}
		_, err := client.Resolve(context.Background(), ChannelStable, m.Version)
		if (err == nil) != (signer == identity) {
			t.Errorf("signer %q: %v", signer, err)
		}
	}
}
