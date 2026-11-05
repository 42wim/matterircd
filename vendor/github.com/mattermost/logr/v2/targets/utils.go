package targets

import (
	"crypto/x509"
	"encoding/base64"
	"errors"
	"os"
)

const (
	DefaultCertKey = "LOGR_DEFAULT_CERT"
)

// GetCertPoolOrNil returns a x509.CertPool containing the cert(s) from `cert`,
// or from the certs specified by the env var `LOGR_DEFAULT_CERT`, either of which
// can be a path to a .pem or .crt file, or a base64 encoded cert.
//
// If a cert is specified by either `cert` or `LOGR_DEFAULT_CERT`, but the cert
// is invalid then an error is returned.
//
// If no certs are specified by either `cert` or `LOGR_DEFAULT_CERT`, then
// nil is returned.
func GetCertPoolOrNil(cert string) (*x509.CertPool, error) {
	if cert == "" {
		cert = getDefaultCert()
		if cert == "" {
			return nil, nil // no cert provided, not an error but no pool returned
		}
	}

	// first treat as a file and try to read.
	serverCert, err := os.ReadFile(cert)
	if err != nil {
		// maybe it's a base64 encoded cert
		serverCert, err = base64.StdEncoding.DecodeString(cert)
		if err != nil {
			return nil, errors.New("cert cannot be read")
		}
	}

	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(serverCert); ok {
		return pool, nil
	}
	return nil, errors.New("cannot parse cert")
}

func getDefaultCert() string {
	return os.Getenv(DefaultCertKey)
}
