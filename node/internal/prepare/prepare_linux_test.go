//go:build linux

package prepare_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"veil.local/node/internal/compose"
	"veil.local/node/internal/config"
	"veil.local/node/internal/prepare"
)

func TestPreparationRoleSeparationAndValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pair")
	o := prepare.Options{Output: dir, ServerAddress: "127.0.0.1:24443", ClientListen: "127.0.0.1:0", LocalTest: true, MinValidity: time.Hour}
	report, err := prepare.Create(o, compose.FileReader(), compose.BundleWriter())
	if err != nil {
		t.Fatal(err)
	}
	var loaded []config.Loaded
	for _, role := range []string{"client", "server"} {
		l, err := config.Load(filepath.Join(dir, role, "node.json"), compose.FileReader())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = compose.New(l); err != nil {
			t.Fatal(err)
		}
		if l.Model.SHA256() != report.ModelSHA256 || l.Report().EndToEnd != "not_checked" || l.Report().Limits.StreamBytes != 8<<20 {
			t.Fatal("configuration binding or validation scope", l.Report())
		}
		loaded = append(loaded, l)
	}
	if loaded[1].Config.ClientFingerprints[0] != loaded[0].Identity.Fingerprint() {
		t.Fatal("client authorization mismatch")
	}
	for _, path := range []string{"server/client-key.pem", "server/client-ca-key.pem", "client/server-key.pem", "client/client-ca-key.pem"} {
		if _, err = os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatal("cross-role private key", path, err)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "admin/client-ca-key.pem")); err != nil {
		t.Fatal("missing offline issuer key", err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "client/client-key.pem"))
	if _, err = prepare.Create(o, compose.FileReader(), compose.BundleWriter()); err == nil {
		t.Fatal("existing pair overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "client/client-key.pem"))
	if !bytes.Equal(before, after) {
		t.Fatal("existing identity changed")
	}
	// Exercise the supplied-server-identity path, including its chain/name
	// verification, rather than testing only the generated local identity.
	o.LocalTest = false
	o.ServerCertificate = filepath.Join(dir, "server/server.pem")
	o.ServerKey = filepath.Join(dir, "server/server-key.pem")
	o.ServerCA = filepath.Join(dir, "client/server-ca.pem")
	o.Output = filepath.Join(t.TempDir(), "provided")
	if _, err = prepare.Create(o, compose.FileReader(), compose.BundleWriter()); err != nil {
		t.Fatal("supplied server identity", err)
	}
	o.ServerName = "wrong.test"
	o.Output = filepath.Join(t.TempDir(), "rejected")
	if _, err = prepare.Create(o, compose.FileReader(), compose.BundleWriter()); err == nil {
		t.Fatal("wrong server identity accepted")
	}
	if _, err = os.Stat(o.Output); !os.IsNotExist(err) {
		t.Fatal("invalid identity published output", err)
	}
	o.ServerName = "127.0.0.1"
	o.MinValidity = 48 * time.Hour
	if _, err = prepare.Create(o, compose.FileReader(), compose.BundleWriter()); err == nil {
		t.Fatal("insufficient certificate horizon accepted")
	}
}

func TestLocalTestCannotPreparePublicListener(t *testing.T) {
	for _, o := range []prepare.Options{
		{ServerAddress: "192.0.2.1:443"},
		{ServerAddress: "127.0.0.1:443", ServerListen: "0.0.0.0:443"},
		{ServerAddress: "127.0.0.1:443", ClientListen: "0.0.0.0:1080"},
	} {
		o.LocalTest = true
		o.Output = filepath.Join(t.TempDir(), "rejected")
		if _, err := prepare.Create(o, compose.FileReader(), compose.BundleWriter()); err == nil {
			t.Fatal("public local-test preparation accepted", o)
		}
	}
}
