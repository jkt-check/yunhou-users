package wechat

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
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
// returns the certificate's serial number as an UPPERCASE HEX string —
// the format WeChat Pay v3 expects in the `serial_no` field of the
// Authorization header. Big-endian byte representation, hex-encoded
// (no `0x` prefix, no sign), uppercase letters per the official
// wechatpay-go SDK's `utils.GetCertificateSerialNumber` reference.
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
	return serialToHexUpper(cert.SerialNumber.Bytes()), nil
}

// serialToHexUpper returns the cert's serial number as the WeChat Pay v3
// "serial_no" field expects: uppercase hex of the big-endian byte
// representation. Matches the official wechatpay-go SDK's
// utils.GetCertificateSerialNumber. We use `%X` (uppercase) because
// wechatpay-apiv3 merchants sometimes register their cert with the
// platform using the upper-case form, and the auth lookup is
// case-sensitive on the merchant side.
//
// (Previously the helper returned decimal — that was wrong; WeChat
// does NOT accept decimal serial numbers in the Authorization header.)
func serialToHexUpper(serial []byte) string {
	return fmt.Sprintf("%X", serial)
}
