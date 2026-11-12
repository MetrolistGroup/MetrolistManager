package githubrelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindAssetAndDownload(t *testing.T) {
	content := []byte("verified payload")
	digest := sha256.Sum256(content)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/MetrolistGroup/Metrolist/releases/tags/v14.0.1":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tag_name":"v14.0.1","draft":false,"prerelease":false,"assets":[{"name":"app.zip","state":"uploaded","size":%d,"digest":"sha256:%s","browser_download_url":"%s/app.zip"}]}`,
				len(content), hex.EncodeToString(digest[:]), server.URL)
		case "/app.zip":
			w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient("test")
	client.HTTPClient = server.Client()
	client.APIBaseURL = server.URL
	asset, err := client.FindAsset(context.Background(), "MetrolistGroup", "Metrolist", "v14.0.1", "app.zip")
	if err != nil {
		t.Fatalf("FindAsset() error = %v", err)
	}
	destination := filepath.Join(t.TempDir(), "app.zip")
	if err := client.Download(context.Background(), asset, destination, nil); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("download = %q", got)
	}
}

func TestFindAssetRequiresDigest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v14.0.1","draft":false,"prerelease":false,"assets":[{"name":"app.zip","state":"uploaded","size":10,"digest":null,"browser_download_url":"https://example.test/app.zip"}]}`)
	}))
	defer server.Close()
	client := NewClient("test")
	client.HTTPClient = server.Client()
	client.APIBaseURL = server.URL
	if _, err := client.FindAsset(context.Background(), "owner", "repo", "v14.0.1", "app.zip"); err == nil {
		t.Fatal("FindAsset() error = nil, want missing digest error")
	}
}

func TestDownloadRejectsDigestMismatch(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("bad"))
	}))
	defer server.Close()
	asset := Asset{
		Name:   "app.zip",
		URL:    server.URL,
		Digest: "sha256:" + strings.Repeat("0", 64),
		Size:   3,
	}
	client := NewClient("test")
	client.HTTPClient = server.Client()
	if err := client.Download(context.Background(), asset, filepath.Join(t.TempDir(), "app.zip"), nil); err == nil {
		t.Fatal("Download() error = nil, want digest mismatch")
	}
}

func TestFindAssetRejectsInsecureDownloadURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v14.0.1","draft":false,"prerelease":false,"assets":[{"name":"app.zip","state":"uploaded","size":10,"digest":"sha256:`+strings.Repeat("0", 64)+`","browser_download_url":"http://example.test/app.zip"}]}`)
	}))
	defer server.Close()
	client := NewClient("test")
	client.HTTPClient = server.Client()
	client.APIBaseURL = server.URL
	if _, err := client.FindAsset(context.Background(), "owner", "repo", "v14.0.1", "app.zip"); err == nil {
		t.Fatal("FindAsset() accepted insecure download URL")
	}
}
