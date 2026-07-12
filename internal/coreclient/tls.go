package coreclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// TLSConfig holds the file paths for the BFF's mutual-TLS identity when dialing
// Core. All three must be present; the config layer enforces that invariant.
type TLSConfig struct {
	CertFile string // client certificate the BFF presents to Core (PEM)
	KeyFile  string // private key for CertFile (PEM)
	CAFile   string // CA bundle used to verify Core's server certificate (PEM)
}

// transportCredentials builds the gRPC transport credentials for dialing Core. A
// nil tlsCfg yields plaintext (insecure) credentials for the fixture/local path.
// Otherwise it returns mutual-TLS credentials: the BFF presents its client
// certificate and verifies Core's server certificate against the CA bundle.
//
// Core's gRPC edge is a single L4 Service that load-balances across replica
// pods, each presenting its own per-node leaf (SAN n<id>.nodes.cvm.internal), so
// there is no single hostname to pin. We therefore disable Go's default hostname
// verification and verify the presented chain against the CA ourselves - trust
// stays anchored in the CA (the property that matters) while the rotating node
// identity is tolerated.
func transportCredentials(tlsCfg *TLSConfig) (credentials.TransportCredentials, error) {
	if tlsCfg == nil {
		return insecure.NewCredentials(), nil
	}
	cert, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load core client cert (%s / %s): %w", tlsCfg.CertFile, tlsCfg.KeyFile, err)
	}
	caPEM, err := os.ReadFile(tlsCfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read core CA bundle (%s): %w", tlsCfg.CAFile, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("core CA bundle (%s) contained no PEM certificates", tlsCfg.CAFile)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates:          []tls.Certificate{cert},
		RootCAs:               roots,
		MinVersion:            tls.VersionTLS12,
		InsecureSkipVerify:    true, // hostname pinning disabled; verifyChainAgainst restores CA trust
		VerifyPeerCertificate: verifyChainAgainst(roots),
	}), nil
}

// verifyChainAgainst returns a VerifyPeerCertificate callback that verifies
// Core's presented certificate chains to one of the CA roots, without checking
// the hostname (no DNSName in VerifyOptions). This restores real CA-anchored
// trust that InsecureSkipVerify=true would otherwise discard, while tolerating
// the edge's rotating per-node server identity.
func verifyChainAgainst(roots *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("core server presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parse core server leaf certificate: %w", err)
		}
		intermediates := x509.NewCertPool()
		for _, raw := range rawCerts[1:] {
			ic, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("parse core server intermediate certificate: %w", err)
			}
			intermediates.AddCert(ic)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return fmt.Errorf("core server certificate not trusted by configured CA: %w", err)
		}
		return nil
	}
}
