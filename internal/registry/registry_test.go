package registry

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func start(t *testing.T, mod func(*Config)) *Server {
	t.Helper()
	cfg := Config{DataDir: t.TempDir(), ReadAddr: "127.0.0.1:0", WriteAddr: "127.0.0.1:0", Logger: quiet()}
	if mod != nil {
		mod(&cfg)
	}
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s
}

func do(t *testing.T, c *http.Client, method, url string, body []byte, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func status(t *testing.T, c *http.Client, method, url string) int {
	return do(t, c, method, url, nil, nil).StatusCode
}

const ociType = "application/vnd.oci.image.manifest.v1+json"

// push uploads a minimal image (one empty config blob, no layers) as repo:tag
// through base, using the plain registry HTTP API.
func push(t *testing.T, base, repo, tag string) string {
	t.Helper()
	c := http.DefaultClient
	cfgBlob := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"x":%q}`, tag))
	sum := sha256.Sum256(cfgBlob)
	dgst := fmt.Sprintf("sha256:%x", sum)

	resp := do(t, c, "POST", base+"/v2/"+repo+"/blobs/uploads/", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start upload: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if strings.HasPrefix(loc, "/") {
		loc = base + loc
	}
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	resp = do(t, c, "PUT", loc+sep+"digest="+dgst, cfgBlob, map[string]string{"Content-Type": "application/octet-stream"})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("finish upload: %d %s", resp.StatusCode, b)
	}
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`,
		ociType, dgst, len(cfgBlob)))
	resp = do(t, c, "PUT", base+"/v2/"+repo+"/manifests/"+tag, manifest, map[string]string{"Content-Type": ociType})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put manifest: %d %s", resp.StatusCode, b)
	}
	return resp.Header.Get("Docker-Content-Digest")
}

func TestReadOnlyWrapper(t *testing.T) {
	var got []string
	h := ReadOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = append(got, r.Method) }))
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		rec := &fakeRW{h: http.Header{}}
		h.ServeHTTP(rec, mustReq(m))
		wantOK := m == "GET" || m == "HEAD"
		if wantOK && rec.code != 0 && rec.code != 200 || !wantOK && rec.code != 405 {
			t.Errorf("%s: code %d", m, rec.code)
		}
		if !wantOK && rec.h.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: Allow = %q", m, rec.h.Get("Allow"))
		}
	}
	if strings.Join(got, ",") != "GET,HEAD" {
		t.Errorf("handler reached for %v", got)
	}
}

type fakeRW struct {
	h    http.Header
	code int
}

func (f *fakeRW) Header() http.Header         { return f.h }
func (f *fakeRW) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeRW) WriteHeader(c int)           { f.code = c }

func mustReq(method string) *http.Request {
	r, _ := http.NewRequest(method, "http://x/v2/", nil)
	return r
}

func TestTwoListeners(t *testing.T) {
	s := start(t, nil)
	ro, rw := "http://"+s.ReadAddr(), "http://"+s.WriteAddr()
	c := http.DefaultClient

	if got := status(t, c, "GET", ro+"/v2/"); got != 200 {
		t.Errorf("GET /v2/ read: %d", got)
	}
	if got := status(t, c, "GET", rw+"/v2/"); got != 200 {
		t.Errorf("GET /v2/ write: %d", got)
	}
	// writes: refused on the read-only side, accepted on the write side
	if got := status(t, c, "POST", ro+"/v2/x/blobs/uploads/"); got != 405 {
		t.Errorf("POST read: %d", got)
	}
	if got := status(t, c, "POST", rw+"/v2/x/blobs/uploads/"); got != 202 {
		t.Errorf("POST write: %d", got)
	}

	// push through the write listener, pull through the read-only one
	dgst := push(t, rw, "demo/app", "default-20261022-020000")
	push(t, rw, "demo/app", "latest")
	resp := do(t, c, "GET", ro+"/v2/demo/app/manifests/latest", nil, map[string]string{"Accept": ociType})
	if resp.StatusCode != 200 {
		t.Fatalf("pull manifest: %d", resp.StatusCode)
	}
	if got := do(t, c, "HEAD", ro+"/v2/demo/app/manifests/default-20261022-020000", nil, map[string]string{"Accept": ociType}).StatusCode; got != 200 {
		t.Errorf("HEAD manifest: %d", got)
	}
	var tags struct{ Tags []string }
	body, _ := io.ReadAll(do(t, c, "GET", ro+"/v2/demo/app/tags/list", nil, nil).Body)
	if !strings.Contains(string(body), "latest") || !strings.Contains(string(body), "default-20261022-020000") {
		t.Errorf("tags/list = %s %v", body, tags)
	}

	// delete: refused on the read-only side, works on the write side
	if got := status(t, c, "DELETE", ro+"/v2/demo/app/manifests/"+dgst); got != 405 {
		t.Errorf("DELETE read: %d", got)
	}
	if got := status(t, c, "DELETE", rw+"/v2/demo/app/manifests/"+dgst); got != 202 {
		t.Errorf("DELETE write: %d", got)
	}
}

func TestStorageUnderDataDir(t *testing.T) {
	s := start(t, nil)
	push(t, "http://"+s.WriteAddr(), "demo/app", "latest")
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "registry", "docker", "registry", "v2", "repositories", "demo", "app")); err != nil {
		t.Errorf("storage not under DataDir/registry: %v", err)
	}
}

func TestConfigErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, Config{DataDir: t.TempDir(), TLSCert: "c.pem"}); err == nil {
		t.Error("cert without key must fail")
	}
	if _, err := New(ctx, Config{}); err == nil {
		t.Error("missing DataDir must fail")
	}
	if _, err := New(ctx, Config{DataDir: t.TempDir(), TLSCert: "/nonexistent.pem", TLSKey: "/nonexistent.key"}); err == nil {
		t.Error("unreadable cert must fail")
	}
	// port already in use is reported, not swallowed
	s := start(t, nil)
	s2, err := New(ctx, Config{DataDir: t.TempDir(), ReadAddr: s.ReadAddr(), WriteAddr: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Start(); err == nil {
		t.Error("busy read port must fail Start")
	}
}

// --- TLS ---------------------------------------------------------------------------------

func writeCert(t *testing.T, certPath, keyPath string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "prebake-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  nil,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func servedSerial(t *testing.T, addr string) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

func TestTLSServesAndReloadsCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeCert(t, certPath, keyPath, 1)

	s := start(t, func(c *Config) { c.TLSCert, c.TLSKey = certPath, keyPath })
	s.certs.checkEvery = 0 // check on every handshake in the test

	if got := servedSerial(t, s.ReadAddr()); got != 1 {
		t.Fatalf("serial = %d, want 1", got)
	}
	// the registry works over TLS (certificate not verified here)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if got := status(t, c, "GET", "https://"+s.ReadAddr()+"/v2/"); got != 200 {
		t.Errorf("GET over TLS: %d", got)
	}
	if got := status(t, c, "POST", "https://"+s.ReadAddr()+"/v2/x/blobs/uploads/"); got != 405 {
		t.Errorf("POST over TLS: %d", got)
	}
	// the write listener stays plain HTTP
	if got := status(t, http.DefaultClient, "GET", "http://"+s.WriteAddr()+"/v2/"); got != 200 {
		t.Errorf("write listener: %d", got)
	}

	// replace the certificate on disk: the next handshake serves the new one
	future := time.Now().Add(2 * time.Second)
	writeCert(t, certPath, keyPath, 2)
	os.Chtimes(certPath, future, future)
	os.Chtimes(keyPath, future, future)
	if got := servedSerial(t, s.ReadAddr()); got != 2 {
		t.Errorf("after replacement serial = %d, want 2", got)
	}

	// a broken replacement keeps the last good certificate
	later := future.Add(2 * time.Second)
	os.WriteFile(certPath, []byte("garbage"), 0o600)
	os.Chtimes(certPath, later, later)
	if got := servedSerial(t, s.ReadAddr()); got != 2 {
		t.Errorf("after broken replacement serial = %d, want 2", got)
	}
	// and recovers once a valid pair is back
	latest := later.Add(2 * time.Second)
	writeCert(t, certPath, keyPath, 3)
	os.Chtimes(certPath, latest, latest)
	os.Chtimes(keyPath, latest, latest)
	if got := servedSerial(t, s.ReadAddr()); got != 3 {
		t.Errorf("after recovery serial = %d, want 3", got)
	}
}
