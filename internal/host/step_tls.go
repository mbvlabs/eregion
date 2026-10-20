package host

import (
	"context"
	"os"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/engine"
)

type tlsStep struct {
	cfg Config
}

func (s *tlsStep) ID() string                      { return "host-tls-v1" }
func (s *tlsStep) Describe() string                { return "Install the API TLS certificate and private key" }
func (s *tlsStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *tlsStep) certPath() string {
	return filepath.Join(s.cfg.Layout.ConfigDir, "tls", "server.crt")
}
func (s *tlsStep) keyPath() string { return filepath.Join(s.cfg.Layout.ConfigDir, "tls", "server.key") }

func (s *tlsStep) Inputs() ([]byte, error) {
	cert, err := os.ReadFile(s.cfg.APITLSCert) //nolint:gosec // G304: operator-provided TLS certificate.
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(s.cfg.APITLSKey) //nolint:gosec // G304: operator-provided TLS key.
	if err != nil {
		return nil, err
	}
	return canonicalInput(struct {
		CertHash string `json:"cert_hash"`
		KeyHash  string `json:"key_hash"`
		CertPath string `json:"cert_path"`
		KeyPath  string `json:"key_path"`
	}{contentSHA256(cert), contentSHA256(key), s.certPath(), s.keyPath()})
}

func (s *tlsStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	cert, err := os.ReadFile(s.cfg.APITLSCert) //nolint:gosec // G304: operator-provided TLS certificate.
	if err != nil {
		return engine.CheckResult{}, err
	}
	key, err := os.ReadFile(s.cfg.APITLSKey) //nolint:gosec // G304: operator-provided TLS key.
	if err != nil {
		return engine.CheckResult{}, err
	}
	certOK, _, err := sameFileContent(s.certPath(), cert)
	if err != nil {
		return engine.CheckResult{}, err
	}
	keyOK, keyMode, err := sameFileContent(s.keyPath(), key)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !certOK || !keyOK || keyMode != 0o600 {
		return incomplete("API TLS material is missing or differs from the supplied certificate/key")
	}
	return complete("API TLS material is installed with a 0600 private key")
}

func (s *tlsStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	cert, err := os.ReadFile(s.cfg.APITLSCert) //nolint:gosec // G304: operator-provided TLS certificate.
	if err != nil {
		return engine.ApplyResult{}, err
	}
	key, err := os.ReadFile(s.cfg.APITLSKey) //nolint:gosec // G304: operator-provided TLS key.
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writePublicFile(s.certPath(), cert, "root", s.cfg.Layout.Group); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFile(s.keyPath(), key, 0o600, s.cfg.Layout.User, s.cfg.Layout.Group); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Notes: []string{"API TLS material installed under " + filepath.Dir(s.certPath())}, Result: map[string]any{"cert": s.certPath(), "key": s.keyPath()}}, nil
}

func (s *tlsStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	return []string{"+ install " + s.cfg.APITLSCert + " -> " + s.certPath(), "+ install the private key -> " + s.keyPath() + " (0600)"}, nil
}

func (s *tlsStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
