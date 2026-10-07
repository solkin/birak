// Package cluster provides the authenticated network and administration layer
// for quorum storage. Node and operator certificates have distinct URI roles.
package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func ValidID(id string) bool { return identifier.MatchString(id) }

type Principal struct{ Cluster, Role, ID string }

func CertificatePrincipal(cert *x509.Certificate) (Principal, error) {
	if len(cert.URIs) != 1 {
		return Principal{}, errors.New("certificate must have exactly one Birak URI identity")
	}
	u := cert.URIs[0]
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if u.Scheme != "birak" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !ValidID(u.Host) || len(parts) != 2 || (parts[0] != "node" && parts[0] != "admin") || !ValidID(parts[1]) {
		return Principal{}, errors.New("invalid Birak certificate identity")
	}
	return Principal{Cluster: u.Host, Role: parts[0], ID: parts[1]}, nil
}

type Credentials struct {
	Certificate tls.Certificate
	Roots       *x509.CertPool
	Principal   Principal
}

func LoadCredentials(ca, cert, key string, expected Principal) (*Credentials, error) {
	b, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(b) {
		return nil, errors.New("invalid cluster CA")
	}
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, err
	}
	p, err := CertificatePrincipal(leaf)
	if err != nil || p != expected {
		return nil, errors.New("local TLS identity differs from configuration")
	}
	intermediates := x509.NewCertPool()
	for _, der := range c.Certificate[1:] {
		parsed, e := x509.ParseCertificate(der)
		if e != nil {
			return nil, e
		}
		intermediates.AddCert(parsed)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if p.Role == "admin" && usage == x509.ExtKeyUsageServerAuth {
			continue
		}
		if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return nil, err
		}
	}
	return &Credentials{Certificate: c, Roots: roots, Principal: p}, nil
}

func (c *Credentials) ServerTLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{c.Certificate}, ClientCAs: c.Roots, ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("missing peer certificate")
			}
			p, err := CertificatePrincipal(cs.PeerCertificates[0])
			if err != nil {
				return err
			}
			if p.Cluster != c.Principal.Cluster {
				return errors.New("foreign cluster certificate")
			}
			return nil
		}}
}

// Certificate chains, expiry, EKU and URI identity are verified explicitly.
// DNS is a routing address; only the CA-signed node identity grants authority.
func (c *Credentials) ClientTLS(node string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{c.Certificate}, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("missing server certificate")
			}
			intermediates := x509.NewCertPool()
			for _, cert := range cs.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			leaf := cs.PeerCertificates[0]
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: c.Roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return err
			}
			p, err := CertificatePrincipal(leaf)
			if err != nil {
				return err
			}
			if p != (Principal{Cluster: c.Principal.Cluster, Role: "node", ID: node}) {
				return fmt.Errorf("unexpected server identity")
			}
			return nil
		}}
}
