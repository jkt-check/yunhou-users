package wechat

import (
	"os"
	"path/filepath"
	"testing"
)

// cert_edges_test.go — Task 16 覆盖率补强：私钥/证书加载的错误分支
// （缺文件/非 PEM/类型不符/解析失败）。

func TestLoadPrivateKey_ErrorBranches(t *testing.T) {
	if _, err := LoadPrivateKey(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("missing file must error")
	}
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(notPEM, []byte("not a pem file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(notPEM); err == nil {
		t.Error("non-PEM must error")
	}
	wrongType := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(wrongType, []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(wrongType); err == nil {
		t.Error("non-key PEM must error")
	}
	badKey := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badKey, []byte("-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(badKey); err == nil {
		t.Error("unparseable key must error")
	}
}

func TestLoadCertSerial_ErrorBranches(t *testing.T) {
	if _, err := LoadCertSerial(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("missing file must error")
	}
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(notPEM, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSerial(notPEM); err == nil {
		t.Error("non-PEM must error")
	}
	wrongType := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(wrongType, []byte("-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSerial(wrongType); err == nil {
		t.Error("non-CERTIFICATE PEM must error")
	}
	badCert := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badCert, []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCertSerial(badCert); err == nil {
		t.Error("unparseable cert must error")
	}
}
