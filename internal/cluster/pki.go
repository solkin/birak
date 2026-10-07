package cluster

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
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// CreateCA and IssueCertificate are offline administration helpers. They never
// overwrite credentials and never print private key material. Keep ca.key off
// serving nodes; it authorizes both membership and operator certificates.
func CreateCA(dir, cluster string) error {
	if !ValidID(cluster) {
		return errors.New("invalid cluster ID")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Birak CA " + cluster}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, MaxPathLenZero: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return err
	}
	return writePair(dir, "ca", der, key)
}

func IssueCertificate(dir string, p Principal, hosts []string) error {
	if !ValidID(p.Cluster) || !ValidID(p.ID) || (p.Role != "node" && p.Role != "admin") {
		return errors.New("invalid certificate identity")
	}
	b, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return errors.New("invalid CA certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if !ca.IsCA || ca.Subject.CommonName != "Birak CA "+p.Cluster {
		return errors.New("CA belongs to another cluster")
	}
	b, err = os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return err
	}
	block, _ = pem.Decode(b)
	if block == nil {
		return errors.New("invalid CA key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	caKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("unsupported CA key")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: p.Role + "/" + p.ID}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().AddDate(0, 3, 0), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{{Scheme: "birak", Host: p.Cluster, Path: "/" + p.Role + "/" + p.ID}}}
	if p.Role == "node" {
		cert.ExtKeyUsage = append(cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			cert.IPAddresses = append(cert.IPAddresses, ip)
		} else if host != "" {
			cert.DNSNames = append(cert.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writePair(dir, p.Role+"-"+p.ID, der, key)
}

func writePair(dir, name string, der []byte, key *ecdsa.PrivateKey) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	for _, suffix := range []string{".key", ".crt"} {
		if _, err := os.Lstat(filepath.Join(dir, name+suffix)); !os.IsNotExist(err) {
			return errors.New("credential file already exists or cannot be inspected")
		}
	}
	for _, part := range []struct {
		suffix, kind string
		data         []byte
	}{{".key", "PRIVATE KEY", keyDER}, {".crt", "CERTIFICATE", der}} {
		f, err := os.OpenFile(filepath.Join(dir, name+part.suffix), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		err = pem.Encode(f, &pem.Block{Type: part.kind, Bytes: part.data})
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	return nil
}
