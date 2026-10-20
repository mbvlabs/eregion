package telemetry

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// TLSMaterial is the public CA, the server certificate, and the server key used
// by the telemetry OTLP receiver. The key is secret and is written 0600; the CA
// and certificate are public and must be copied to application/builder agents.
type TLSMaterial struct {
	CAPEM   []byte
	CertPEM []byte
	KeyPEM  []byte
}

// loadOrCreateTLS reads the telemetry node's TLS material, or creates a private
// CA and an IP-SAN server certificate when no material exists yet. Once written,
// the same material is reused so agent trust survives retries and reinstalls.
func loadOrCreateTLS(root, address string) (TLSMaterial, error) {
	caPath, certPath, keyPath := tlsPaths(root)
	_, caErr := os.Stat(caPath)
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if caErr == nil && certErr == nil && keyErr == nil {
		material, err := readTLSMaterial(caPath, certPath, keyPath, address)
		if err != nil {
			return TLSMaterial{}, err
		}
		return material, nil
	}
	if !errors.Is(caErr, os.ErrNotExist) && caErr != nil || !errors.Is(certErr, os.ErrNotExist) && certErr != nil || !errors.Is(keyErr, os.ErrNotExist) && keyErr != nil {
		return TLSMaterial{}, contract.NewFault(contract.Unavailable, "telemetry TLS material is unreadable", "Inspect the telemetry root and TLS file permissions.")
	}
	return generateTLS(address)
}

func tlsPaths(root string) (ca, cert, key string) {
	base := root + "/tls"
	return base + "/ca.crt", base + "/server.crt", base + "/server.key"
}

func readTLSMaterial(caPath, certPath, keyPath, address string) (TLSMaterial, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Unavailable, "telemetry CA certificate is unreadable", "Check the telemetry root and TLS file permissions.")
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Unavailable, "telemetry server certificate is unreadable", "Check the telemetry root and TLS file permissions.")
	}
	info, err := os.Stat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return TLSMaterial{}, contract.NewFault(contract.PreconditionFailed, "telemetry TLS key is not a private regular file", "Keep server.key mode 0600; do not continue with an unsafe key.")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Unavailable, "telemetry TLS key is unreadable", "Check the telemetry root and TLS file permissions.")
	}
	if err := verifyTLSMaterial(caPEM, certPEM, keyPEM, address); err != nil {
		return TLSMaterial{}, err
	}
	return TLSMaterial{CAPEM: caPEM, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

func verifyTLSMaterial(caPEM, certPEM, keyPEM []byte, address string) error {
	ca, cert, key, err := parseTLSMaterial(caPEM, certPEM, keyPEM)
	if err != nil {
		return err
	}
	ip := net.ParseIP(address)
	if ip == nil || !certHasIP(cert, ip) {
		return contract.NewFault(contract.PreconditionFailed, "telemetry server certificate does not cover the private node address", "Rotate the TLS material with the current node private address.")
	}
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "telemetry server certificate is not signed by the configured CA", "Rotate the TLS material; do not trust an unrelated certificate.")
	}
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
		return contract.NewFault(contract.PreconditionFailed, "telemetry TLS material is not ECDSA", "Regenerate the telemetry TLS material.")
	}
	if _, ok := key.Public().(*ecdsa.PublicKey); !ok {
		return contract.NewFault(contract.PreconditionFailed, "telemetry TLS key is not ECDSA", "Regenerate the telemetry TLS material.")
	}
	return nil
}

func parseTLSMaterial(caPEM, certPEM, keyPEM []byte) (*x509.Certificate, *x509.Certificate, *ecdsa.PrivateKey, error) {
	caBlock, _ := pem.Decode(caPEM)
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if caBlock == nil || certBlock == nil || keyBlock == nil {
		return nil, nil, nil, contract.NewFault(contract.PreconditionFailed, "telemetry TLS material is not valid PEM", "Rotate the TLS material; do not continue with unreadable certificates.")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || !ca.IsCA {
		return nil, nil, nil, contract.NewFault(contract.PreconditionFailed, "telemetry CA certificate is invalid", "Rotate the TLS material.")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, nil, contract.NewFault(contract.PreconditionFailed, "telemetry server certificate is invalid", "Rotate the TLS material.")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, nil, contract.NewFault(contract.PreconditionFailed, "telemetry TLS key is invalid", "Rotate the TLS material.")
	}
	return ca, cert, key, nil
}

func certHasIP(cert *x509.Certificate, ip net.IP) bool {
	for _, candidate := range cert.IPAddresses {
		if candidate.Equal(ip) {
			return true
		}
	}
	return false
}

func generateTLS(address string) (TLSMaterial, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return TLSMaterial{}, contract.NewFault(contract.ValidationFailed, "invalid telemetry private address", "Use a private IPv4 or IPv6 address.")
	}
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Internal, "could not generate telemetry CA key", "Inspect process entropy; do not continue with a partial install.")
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{CommonName: "eregion telemetry CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Internal, "could not generate telemetry CA certificate", "Inspect process entropy; do not continue with a partial install.")
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Internal, "could not generate telemetry server key", "Inspect process entropy; do not continue with a partial install.")
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serialNumber(),
		Subject:      pkix.Name{CommonName: "eregion telemetry"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
		DNSNames:     []string{"otel-collector", "localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Internal, "could not generate telemetry server certificate", "Inspect process entropy; do not continue with a partial install.")
	}
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return TLSMaterial{}, contract.NewFault(contract.Internal, "could not encode telemetry server key", "Inspect process entropy; do not continue with a partial install.")
	}
	return TLSMaterial{
		CAPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func serialNumber() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	value, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return value
}

func validateCAFile(raw []byte) error {
	block, _ := pem.Decode(raw)
	if block == nil {
		return contract.NewFault(contract.ValidationFailed, "TLS CA file is not valid PEM", "Provide the public ca.crt from the telemetry node.")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return contract.NewFault(contract.ValidationFailed, "TLS CA file is not a CA certificate", "Provide the public ca.crt from the telemetry node.")
	}
	if time.Now().After(cert.NotAfter) {
		return contract.NewFault(contract.PreconditionFailed, "TLS CA certificate is expired", "Rotate the telemetry TLS material and redistribute the CA.")
	}
	return nil
}
