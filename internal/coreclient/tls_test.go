package coreclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/corepb/querypb"
)

// certPair is a generated certificate plus its private key.
type certPair struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

func (c certPair) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})
}

func (c certPair) keyPEM(t *testing.T) []byte {
	b, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
}

// genCA mints a self-signed CA certificate.
func genCA(t *testing.T, cn string) certPair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return certPair{cert: cert, der: der, key: key}
}

// genLeaf issues a leaf signed by ca with the given SAN and EKU.
func genLeaf(t *testing.T, ca certPair, cn, dnsSAN string, eku []x509.ExtKeyUsage) certPair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
		DNSNames:     []string{dnsSAN},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return certPair{cert: cert, der: der, key: key}
}

// writeFiles writes a client cert/key and CA bundle into a temp dir, returning a
// TLSConfig that points at them (mirroring the on-disk secret mount in the pod).
func writeFiles(t *testing.T, client certPair, caPEM []byte) *TLSConfig {
	t.Helper()
	dir := t.TempDir()
	cfg := &TLSConfig{
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
		CAFile:   filepath.Join(dir, "ca.crt"),
	}
	mustWrite(t, cfg.CertFile, client.certPEM())
	mustWrite(t, cfg.KeyFile, client.keyPEM(t))
	mustWrite(t, cfg.CAFile, caPEM)
	return cfg
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// startMTLSCore starts a gRPC server that requires and verifies a client cert
// signed by clientCA and presents serverCert. It mirrors Core's edge: mandatory
// mTLS, and every Query returns UNIMPLEMENTED.
func startMTLSCore(t *testing.T, serverCert certPair, clientCA certPair) string {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	tlsCert := tls.Certificate{Certificate: [][]byte{serverCert.der}, PrivateKey: serverCert.key}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	})))
	querypb.RegisterQueryServiceServer(srv, &stubQueryServer{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// TestMTLSCheckCoreReachesCore proves the whole fix: a BFF built with mTLS
// credentials completes the handshake against a mandatory-mTLS Core and its
// readiness ping reaches the application layer (UNIMPLEMENTED -> CheckOK). The
// server cert's SAN is a node name while we dial 127.0.0.1, proving hostname is
// intentionally not pinned (the edge rotates per-node identities).
func TestMTLSCheckCoreReachesCore(t *testing.T) {
	ca := genCA(t, "calystral-ca-test")
	server := genLeaf(t, ca, "n0", "n0.nodes.cvm.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	client := genLeaf(t, ca, "studio-bff", "studio-bff.clients.cvm.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})

	addr := startMTLSCore(t, server, ca)
	tlsCfg := writeFiles(t, client, ca.certPEM())

	signer, _ := auth.NewPrincipalSigner("")
	c, err := NewGRPCClient(addr, signer, Options{TLS: tlsCfg})
	if err != nil {
		t.Fatalf("new mtls client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if got := c.CheckCore(context.Background()); got != CheckOK {
		t.Fatalf("CheckCore = %q, want ok (mTLS handshake should reach Core)", got)
	}
}

// TestMTLSWrongCARejected proves verifyChainAgainst enforces CA trust: when the
// BFF's configured CA does not include the server's issuer, the handshake fails
// and readiness is unavailable (rather than silently trusting any server).
func TestMTLSWrongCARejected(t *testing.T) {
	ca := genCA(t, "real-ca")
	otherCA := genCA(t, "attacker-ca")
	server := genLeaf(t, ca, "n0", "n0.nodes.cvm.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	client := genLeaf(t, ca, "studio-bff", "studio-bff.clients.cvm.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})

	addr := startMTLSCore(t, server, ca)
	// Configure the BFF to trust the WRONG CA for server verification.
	tlsCfg := writeFiles(t, client, otherCA.certPEM())

	signer, _ := auth.NewPrincipalSigner("")
	c, err := NewGRPCClient(addr, signer, Options{TLS: tlsCfg})
	if err != nil {
		t.Fatalf("new mtls client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if got := c.CheckCore(context.Background()); got != CheckUnavailable {
		t.Fatalf("CheckCore = %q, want unavailable (server cert must not be trusted)", got)
	}
}

// TestTransportCredentialsErrors covers the misconfiguration paths.
func TestTransportCredentialsErrors(t *testing.T) {
	// nil TLS -> plaintext insecure creds, no error.
	if creds, err := transportCredentials(nil); err != nil || creds == nil {
		t.Fatalf("nil TLS: creds=%v err=%v, want insecure creds", creds, err)
	}
	// Missing files -> error.
	if _, err := transportCredentials(&TLSConfig{CertFile: "/no/such.crt", KeyFile: "/no/such.key", CAFile: "/no/such.ca"}); err == nil {
		t.Fatal("expected error for missing cert files")
	}
	// CA file with no PEM certs -> error.
	ca := genCA(t, "ca")
	client := genLeaf(t, ca, "c", "c.test", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	bad := writeFiles(t, client, []byte("not a pem"))
	if _, err := transportCredentials(bad); err == nil {
		t.Fatal("expected error for CA bundle with no certificates")
	}
}
