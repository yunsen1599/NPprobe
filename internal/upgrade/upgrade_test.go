package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func tarball(t *testing.T, name string, content []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestResolveAndApply(t *testing.T) {
	pkg := fmt.Sprintf("npprobe-linux-%s.tar.gz", runtime.GOARCH)
	archive := tarball(t, "npprobe", []byte("NEW-BINARY"))
	sum := sha256.Sum256(archive)
	badSums := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r/releases/latest":
			w.Header().Set("Location", "https://github.com/o/r/releases/tag/v0.2.0")
			w.WriteHeader(http.StatusFound)
		case "/o/r/releases/download/0.2.0/checksums.txt":
			h := hex.EncodeToString(sum[:])
			if badSums {
				h = "00"
			}
			fmt.Fprintf(w, "%s  %s\n", h, pkg)
		case "/o/r/releases/download/0.2.0/" + pkg:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "npprobe")
	_ = os.WriteFile(exe, []byte("OLD"), 0o755)
	u := &Upgrader{Repo: "o/r", Current: "0.1.0", Base: srv.URL, Exe: exe}

	v, err := u.Resolve(context.Background(), "latest")
	if err != nil || v != "0.2.0" {
		t.Fatalf("resolve = %q %v", v, err)
	}
	if !u.Needed(v) || u.Needed("0.1.0") || (&Upgrader{Current: "dev"}).Needed("0.2.0") {
		t.Fatal("Needed")
	}
	if runtime.GOOS != "linux" {
		if err := u.Apply(context.Background(), v); err == nil {
			t.Fatal("non-linux must refuse")
		}
		// 在非 linux 上直接测下载 / 校验 / 替换的其余部分
		bin, err := extract(archive, "npprobe")
		if err != nil || string(bin) != "NEW-BINARY" {
			t.Fatalf("extract: %q %v", bin, err)
		}
		if err := u.replace(bin); !errors.Is(err, ErrRestart) {
			t.Fatal(err)
		}
	} else {
		badSums = true
		if err := u.Apply(context.Background(), v); err == nil {
			t.Fatal("checksum mismatch must fail")
		}
		badSums = false
		if err := u.Apply(context.Background(), v); !errors.Is(err, ErrRestart) {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(exe); string(got) != "NEW-BINARY" {
		t.Fatalf("binary not replaced: %q", got)
	}
	if _, err := checksumFor([]byte("ab  other.tar.gz\n"), pkg); err == nil {
		t.Fatal("missing checksum must fail")
	}
}
