package emailaccounts

import (
	"crypto/x509"
	"fmt"
	"os"
)

// LoadTLSRootCAs returns the system trust pool extended with the configured
// CA file. A blank path means system roots only.
func LoadTLSRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("emailaccounts: read tls_ca_file %q: %w", path, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("emailaccounts: no valid PEM certificates in tls_ca_file %q", path)
	}
	return pool, nil
}
