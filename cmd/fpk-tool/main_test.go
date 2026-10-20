package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func iconPNG(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, size, size))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	app := tar.NewWriter(gz)
	elf := make([]byte, 20)
	copy(elf, "\x7fELF")
	elf[4] = 2
	elf[5] = 1
	binary.LittleEndian.PutUint16(elf[18:], 62)
	for name, data := range map[string][]byte{"bin/micast-go": elf, "audio/ffmpeg": elf, "ui/config": []byte(`{}`), "ui/images/icon_64.png": iconPNG(t, 64), "ui/images/icon_128.png": iconPNG(t, 128), "ui/images/icon_256.png": iconPNG(t, 256), "licenses/ffmpeg/LICENSE.txt": []byte("license")} {
		app.WriteHeader(&tar.Header{Name: name, Mode: 0666, Size: int64(len(data)), Typeflag: tar.TypeReg})
		app.Write(data)
	}
	app.Close()
	gz.Close()
	file, err := os.Create(filepath.Join(dir, "raw.fpk"))
	if err != nil {
		t.Fatal(err)
	}
	outerGzip := gzip.NewWriter(file)
	outer := tar.NewWriter(outerGzip)
	for name, data := range map[string][]byte{"app.tgz": payload.Bytes(), "manifest": []byte("appname = old\r\nchecksum=old\r\n"), "cmd/main": []byte("#!/bin/bash\n"), "cmd/install_callback": []byte("exit 0\n"), "config/privilege": []byte(`{}`), "ICON.PNG": iconPNG(t, 64), "ICON_256.PNG": iconPNG(t, 256)} {
		outer.WriteHeader(&tar.Header{Name: name, Mode: 0666, Size: int64(len(data)), Typeflag: tar.TypeReg})
		outer.Write(data)
	}
	outer.Close()
	outerGzip.Close()
	file.Close()
	source := filepath.Join(dir, "manifest")
	if err = os.WriteFile(source, []byte("appname=micast-go\nversion=0.1.0\nplatform=x86\ndisplay_name=MiCast Go\ndesc=全屋播放\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return file.Name(), source
}
func TestNormalizeRestoresUTF8PermissionsAndChecksum(t *testing.T) {
	raw, source := fixture(t)
	output := filepath.Join(filepath.Dir(raw), "result.fpk")
	if err := normalize(raw, output, source, "x86"); err != nil {
		t.Fatal(err)
	}
	if err := validate(output, "x86"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	reader, closer, err := outerReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "manifest" {
			b, _ := io.ReadAll(archive)
			if !strings.Contains(string(b), "全屋播放") {
				t.Fatal("UTF-8 description lost")
			}
		}
		if header.Name == "cmd/main" && header.Mode&0111 == 0 {
			t.Fatal("script not executable")
		}
	}
}
func TestInvalidArchitectureDoesNotPublishArtifact(t *testing.T) {
	raw, source := fixture(t)
	output := filepath.Join(filepath.Dir(raw), "result.fpk")
	if err := normalize(raw, output, source, "arm"); err == nil {
		t.Fatal("wrong architecture accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("bad package published")
	}
}
func TestUnsafeArchivePathsRejected(t *testing.T) {
	for _, name := range []string{"../secret", "/absolute", "cmd/../../secret", "app\\secret"} {
		if _, err := clean(name); err == nil {
			t.Fatal("unsafe archive path", name)
		}
	}
}

func TestIconValidationRejectsWrongSizeAndFormat(t *testing.T) {
	if err := validateIcon(iconPNG(t, 64), 64); err != nil {
		t.Fatal(err)
	}
	if err := validateIcon(iconPNG(t, 32), 64); err == nil {
		t.Fatal("wrong icon size accepted")
	}
	if err := validateIcon([]byte("png"), 64); err == nil {
		t.Fatal("non-PNG icon accepted")
	}
}

func TestNormalizeManifestPreservesMultilineDescription(t *testing.T) {
	source := "appname=micast-go\r\nplatform=x86\r\ndesc=\"\"\"全屋播放\r\n\r\nplatform=description text\r\nchecksum=description text\r\n了解 MiCast：https://github.com/DyMode/MiCast\r\n\"\"\"\r\nchecksum=old\r\n"
	got, err := normalizeManifest([]byte(source), "arm", "new")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(source, "\r\n", "\n")
	want = strings.Replace(want, "platform=x86", "platform=arm", 1)
	want = strings.Replace(want, "checksum=old\n", "\nchecksum=new\n", 1)
	if string(got) != want {
		t.Fatalf("manifest changed unexpectedly:\n%s", got)
	}
}

func TestNormalizeRejectsUnterminatedDescription(t *testing.T) {
	raw, source := fixture(t)
	if err := os.WriteFile(source, []byte("appname=micast-go\nplatform=x86\ndesc=\"\"\"broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(raw), "result.fpk")
	if err := normalize(raw, output, source, "x86"); err == nil {
		t.Fatal("unterminated description accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("bad package published")
	}
}
