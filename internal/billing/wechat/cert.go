package wechat

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
)

// LoadPrivateKey reads a PEM-encoded RSA private key from disk.
// Supports both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY")
// blocks; wechat Signer takes either. Returns the parsed key.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#1: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8: %w", err)
		}
		rsaKey, ok := keyAny.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is %T, not RSA", keyAny)
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q (want RSA PRIVATE KEY or PRIVATE KEY)", block.Type)
	}
}

// LoadCertSerial reads a PEM-encoded X.509 certificate from disk and
// returns the certificate's serial number as a decimal string — the
// format WeChat Pay v3 expects in the `serial_no` field of the
// Authorization header. Big-endian byte representation, decimal-encoded.
func LoadCertSerial(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cert: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", fmt.Errorf("no PEM block found in %s", path)
	}
	if block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("unsupported PEM block type %q (want CERTIFICATE)", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse cert: %w", err)
	}
	return serialToDecimal(cert.SerialNumber), nil
}

// serialToDecimal converts a big.Int serial number to WeChat's expected
// decimal string form. cert.SerialNumber.String() (the natural big.Int
// representation) does NOT match what WeChat expects — WeChat requires
// the byte representation interpreted as a positive integer with no
// sign prefix. For positive serials (the common case), this is the same
// as .String(); for serials whose high bit is set, .String() prepends a
// sign and breaks WeChat's parser.
func serialToDecimal(serial *big.Int) string {
	return fmt.Sprintf("%d", new(big.Int).Abs(serial))
}