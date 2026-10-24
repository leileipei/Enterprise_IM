package localoidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type TLSFiles struct {
	CACert, CAKey, ServerCert, ServerKey, Fingerprint string
	NotAfter                                          time.Time
}

func PrivateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("canonical private directory required")
	}
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil || resolved != path {
		return errors.New("linked private directory")
	}
	info, e := os.Lstat(path)
	if e != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("private directory permissions required")
	}
	return nil
}
func PrivateFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("canonical private file required")
	}
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil || resolved != path {
		return errors.New("linked private file")
	}
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("private file permissions required")
	}
	return nil
}
func GenerateTLS(dir, owner string, now time.Time) (files TLSFiles, err error) {
	if PrivateDirectory(dir) != nil || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(owner) {
		return files, errors.New("invalid local TLS ownership")
	}
	files = TLSFiles{CACert: filepath.Join(dir, "ca.pem"), CAKey: filepath.Join(dir, "ca.key"), ServerCert: filepath.Join(dir, "server.pem"), ServerKey: filepath.Join(dir, "server.key"), NotAfter: now.UTC().Add(24 * time.Hour)}
	paths := []string{files.CACert, files.CAKey, files.ServerCert, files.ServerKey}
	for _, p := range paths {
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			return TLSFiles{}, errors.New("local TLS output already exists")
		}
	}
	caKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return TLSFiles{}, errors.New("local CA key unavailable")
	}
	serverKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return TLSFiles{}, errors.New("local server key unavailable")
	}
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
	caSerial, e := serial()
	if e != nil {
		return TLSFiles{}, errors.New("local TLS serial unavailable")
	}
	serverSerial, e := serial()
	if e != nil {
		return TLSFiles{}, errors.New("local TLS serial unavailable")
	}
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "Enterprise IM local test " + owner}, NotBefore: now.UTC().Add(-time.Minute), NotAfter: files.NotAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, e := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if e != nil {
		return TLSFiles{}, errors.New("local CA unavailable")
	}
	leaf := &x509.Certificate{SerialNumber: serverSerial, Subject: pkix.Name{CommonName: "Enterprise IM local loopback"}, NotBefore: ca.NotBefore, NotAfter: files.NotAfter, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	leafDER, e := x509.CreateCertificate(rand.Reader, leaf, ca, &serverKey.PublicKey, caKey)
	if e != nil {
		return TLSFiles{}, errors.New("local server certificate unavailable")
	}
	caBytes, e := x509.MarshalECPrivateKey(caKey)
	if e != nil {
		return TLSFiles{}, errors.New("local CA encoding unavailable")
	}
	serverBytes, e := x509.MarshalECPrivateKey(serverKey)
	if e != nil {
		return TLSFiles{}, errors.New("local server encoding unavailable")
	}
	data := [][]byte{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caBytes}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverBytes})}
	created := []string{}
	defer func() {
		if err != nil {
			for _, p := range created {
				_ = os.Remove(p)
			}
		}
	}()
	for i, path := range paths {
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return TLSFiles{}, errors.New("local TLS output unavailable")
		}
		created = append(created, path)
		_, writeErr := f.Write(data[i])
		syncErr := f.Sync()
		closeErr := f.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return TLSFiles{}, errors.New("local TLS write failed")
		}
	}
	sum := sha256.Sum256(caDER)
	files.Fingerprint = hex.EncodeToString(sum[:])
	return files, nil
}
