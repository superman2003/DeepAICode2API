package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureArchive(t *testing.T, extra string, wrongHash bool) []byte {
	t.Helper()
	files := map[string][]byte{"bin/deepaicode2api.exe": []byte("test-gateway"), "bin/register.exe": []byte("test-register")}
	manifest := Manifest{Version: "v0.2.0", OS: "windows", Arch: "amd64", Protocol: 1}
	for _, name := range []string{"bin/deepaicode2api.exe", "bin/register.exe"} {
		hash := sha256.Sum256(files[name])
		digest := hex.EncodeToString(hash[:])
		if wrongHash {
			digest = strings.Repeat("0", 64)
		}
		manifest.Files = append(manifest.Files, File{Path: name, SHA256: digest})
	}
	raw, _ := json.Marshal(manifest)
	files["update-manifest.json"] = raw
	if extra != "" {
		files[extra] = []byte("forbidden")
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, data := range files {
		entry, _ := writer.Create(name)
		_, _ = entry.Write(data)
	}
	writer.Close()
	return buffer.Bytes()
}

func TestExtractOnlyProgramFilesAndVerifyHashes(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		wrong       bool
		allowed     bool
	}{{"valid", "", false, true}, {"env", ".env", false, false}, {"data", "data/accounts.json", false, false}, {"traversal", "../escape.exe", false, false}, {"wrong-hash", "", true, false}} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "update.zip")
			os.WriteFile(archive, fixtureArchive(t, test.extra, test.wrong), 0600)
			_, err := Extract(archive, filepath.Join(dir, "stage"), "v0.2.0")
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v error=%v", test.allowed, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "escape.exe")); err == nil {
				t.Fatal("archive escaped staging directory")
			}
		})
	}
}

func TestReleaseCheckPrepareAndApplyPreserveConfiguration(t *testing.T) {
	t.Setenv("DEEPAI2API_MANAGED_UPDATES", "1")
	root := t.TempDir()
	data := filepath.Join(root, "data")
	os.MkdirAll(data, 0700)
	environment := []byte("PRIVATE_MASTER=do-not-replace\n")
	os.WriteFile(filepath.Join(root, ".env"), environment, 0600)
	manager, err := New(root, data, "v0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	archive := fixtureArchive(t, "", false)
	hash := sha256.Sum256(archive)
	release := Release{Tag: "v0.2.0", URL: "https://github.com/" + DefaultRepository + "/releases/tag/v0.2.0", Assets: []Asset{{Name: AssetName, URL: "https://github.com/" + DefaultRepository + "/releases/download/v0.2.0/" + AssetName, Size: int64(len(archive)), Digest: "sha256:" + hex.EncodeToString(hash[:])}}}
	manager.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		if req.URL.Host == "api.github.com" {
			body, _ = json.Marshal(release)
		} else {
			body = archive
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	if state, err := manager.Check(context.Background()); err != nil || state.Phase != "available" {
		t.Fatalf("check: %#v %v", state, err)
	}
	if err := manager.Prepare(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Status().Phase == "ready" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if manager.Status().Phase != "ready" {
		t.Fatalf("prepare: %#v", manager.Status())
	}
	if manager.Status().Supported {
		if err := manager.Apply(3); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(filepath.Join(data, "updater", "request.json"))
		var job Job
		json.Unmarshal(raw, &job)
		if job.Accounts != 3 || job.ProjectRoot != root || len(job.Files) != 2 {
			t.Fatal("invalid apply job")
		}
	}
	after, _ := os.ReadFile(filepath.Join(root, ".env"))
	if !bytes.Equal(after, environment) {
		t.Fatal("updater changed private configuration")
	}
}

func TestUpdaterRejectsUntrustedOriginsAndDowngrades(t *testing.T) {
	for _, address := range []string{"http://github.com/owner/repo", "https://evil.example/file", "https://github.com.evil.example/file", "https://user:pass@github.com/x"} {
		if trustedURL(address) {
			t.Fatalf("trusted hostile URL %q", address)
		}
	}
	for _, repo := range []string{"../repo", "https://github.com/owner/repo", "owner/repo/extra", "owner/../repo"} {
		if validRepository(repo) {
			t.Fatalf("accepted bad repo %q", repo)
		}
	}
	if !newer("v0.2.1", "v0.2.0") || newer("v0.1.9", "v0.2.0") || newer("v0.2.0", "0.2.0") {
		t.Fatal("version downgrade/same-version handling is incorrect")
	}
}
