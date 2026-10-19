// fpk-tool normalizes Linux executable bits after the Windows fnpack build,
// then validates the complete package before it is copied into dist/fnos.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

func main() {
	input := flag.String("input", "", "fnpack output")
	output := flag.String("output", "", "normalized package")
	platform := flag.String("platform", "x86", "x86 or arm")
	verify := flag.Bool("verify", false, "verify only")
	manifest := flag.String("manifest", "", "UTF-8 source manifest")
	flag.Parse()
	var err error
	if *verify {
		err = validate(*input, *platform)
	} else {
		err = normalize(*input, *output, *manifest, *platform)
		if err == nil {
			err = validate(*output, *platform)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("FPK verified:", map[bool]string{true: *input, false: *output}[*verify])
}
func clean(name string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	if name == "." {
		return name, nil
	}
	if name == ".." || strings.HasPrefix(name, "../") || path.IsAbs(name) || path.Clean(name) != strings.TrimSuffix(name, "/") || strings.Contains(name, "\\") {
		return "", errors.New("unsafe archive path")
	}
	return strings.TrimSuffix(name, "/"), nil
}
func executable(name string) bool {
	return name == "bin/micast-go" || name == "audio/ffmpeg" || strings.HasPrefix(name, "cmd/")
}
func rewriteApp(reader io.Reader) (*os.File, error) {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tmp, err := os.CreateTemp("", "micast-go-app-*.tgz")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	writer, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	out := tar.NewWriter(writer)
	in := tar.NewReader(gz)
	for {
		header, err := in.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name, err := clean(header.Name)
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return nil, errors.New("unsupported app archive entry")
		}
		copyHeader := *header
		copyHeader.Mode = 0644
		if header.Typeflag == tar.TypeDir || executable(name) {
			copyHeader.Mode = 0755
		}
		copyHeader.Uid = 0
		copyHeader.Gid = 0
		copyHeader.Uname = "root"
		copyHeader.Gname = "root"
		if err = out.WriteHeader(&copyHeader); err != nil {
			return nil, err
		}
		if _, err = io.Copy(out, in); err != nil {
			return nil, err
		}
	}
	if err = out.Close(); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	if _, err = tmp.Seek(0, 0); err != nil {
		return nil, err
	}
	ok = true
	return tmp, nil
}
func normalize(input, output, sourceManifest, platform string) error {
	if input == "" || output == "" || input == output {
		return errors.New("separate input and output are required")
	}
	file, err := os.Open(input)
	if err != nil {
		return err
	}
	defer file.Close()
	target, err := os.CreateTemp(filepath.Dir(output), ".micast-go-*.fpk")
	if err != nil {
		return err
	}
	defer target.Close()
	defer os.Remove(target.Name())
	manifest, err := os.ReadFile(sourceManifest)
	if err != nil || !utf8.Valid(manifest) {
		return errors.New("valid UTF-8 source manifest is required")
	}
	appChecksum := ""
	compressed := gzip.NewWriter(target)
	defer compressed.Close()
	out := tar.NewWriter(compressed)
	reader, closer, err := outerReader(file)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}
	in := tar.NewReader(reader)
	for {
		header, err := in.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := clean(header.Name)
		if err != nil {
			return err
		}
		h := *header
		if name == "manifest" {
			continue
		}
		h.Mode = 0644
		if h.Typeflag == tar.TypeDir || strings.HasPrefix(name, "cmd/") {
			h.Mode = 0755
		}
		if name == "app.tgz" {
			app, err := rewriteApp(in)
			if err != nil {
				return err
			}
			info, err := app.Stat()
			if err != nil {
				app.Close()
				os.Remove(app.Name())
				return err
			}
			h.Size = info.Size()
			digest := md5.New()
			if err = out.WriteHeader(&h); err == nil {
				_, err = io.Copy(io.MultiWriter(out, digest), app)
			}
			appChecksum = hex.EncodeToString(digest.Sum(nil))
			app.Close()
			os.Remove(app.Name())
			if err != nil {
				return err
			}
		} else {
			if err = out.WriteHeader(&h); err != nil {
				return err
			}
			if _, err = io.Copy(out, in); err != nil {
				return err
			}
		}
	}
	if appChecksum == "" {
		return errors.New("app archive missing")
	}
	b, err := normalizeManifest(manifest, platform, appChecksum)
	if err != nil {
		return err
	}
	if err = out.WriteHeader(&tar.Header{Name: "manifest", Mode: 0644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err = out.Write(b); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = compressed.Close(); err != nil {
		return err
	}
	if err = target.Close(); err != nil {
		return err
	}
	if err = validate(target.Name(), platform); err != nil {
		return err
	}
	return os.Rename(target.Name(), output)
}

// Preserve multiline values verbatim while updating only top-level generated fields.
func normalizeManifest(manifest []byte, platform, checksum string) ([]byte, error) {
	var result strings.Builder
	multiline := false
	for _, line := range strings.Split(strings.ReplaceAll(string(manifest), "\r\n", "\n"), "\n") {
		if multiline {
			result.WriteString(line + "\n")
			if strings.Contains(line, `"""`) {
				multiline = false
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			switch strings.TrimSpace(key) {
			case "checksum":
				continue
			case "platform":
				line = "platform=" + platform
			}
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, `"""`) && !strings.Contains(value[3:], `"""`) {
				multiline = true
			}
		}
		result.WriteString(line + "\n")
	}
	if multiline {
		return nil, errors.New("unterminated multiline manifest value")
	}
	result.WriteString("checksum=" + checksum + "\n")
	return []byte(result.String()), nil
}
func outerReader(file *os.File) (io.Reader, io.Closer, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return nil, nil, err
	}
	if header[0] == 0x1f && header[1] == 0x8b {
		gz, err := gzip.NewReader(file)
		return gz, gz, err
	}
	return file, nil, nil
}

const maxIconBytes = 1024 * 1024

func validateIcon(data []byte, size int) error {
	if len(data) > maxIconBytes {
		return fmt.Errorf("%dx%d icon exceeds 1024 KB", size, size)
	}
	if len(data) < 33 || string(data[:8]) != "\x89PNG\r\n\x1a\n" || string(data[12:16]) != "IHDR" {
		return fmt.Errorf("%dx%d icon is not a valid PNG", size, size)
	}
	width := binary.BigEndian.Uint32(data[16:20])
	height := binary.BigEndian.Uint32(data[20:24])
	if width != uint32(size) || height != uint32(size) {
		return fmt.Errorf("expected %dx%d icon, got %dx%d", size, size, width, height)
	}
	return nil
}

func validate(filename, platform string) error {
	if platform != "x86" && platform != "arm" {
		return errors.New("unknown FPK platform")
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, closer, err := outerReader(file)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}
	in := tar.NewReader(reader)
	found := map[string]bool{}
	appFound := map[string]bool{}
	manifestChecksum, appChecksum := "", ""
	for {
		h, err := in.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := clean(h.Name)
		if err != nil {
			return err
		}
		found[name] = true
		if name == "manifest" {
			b, err := io.ReadAll(io.LimitReader(in, 65536))
			if err != nil {
				return err
			}
			if !utf8.Valid(b) || !strings.Contains(string(b), "appname=micast-go") || !strings.Contains(string(b), "platform="+platform) || strings.Contains(string(b), "install_dep_apps=python") {
				return errors.New("invalid MiCast Go manifest")
			}
			if _, err := normalizeManifest(b, platform, ""); err != nil {
				return err
			}
			for _, line := range strings.Split(string(b), "\n") {
				key, value, ok := strings.Cut(line, "=")
				if ok && strings.TrimSpace(key) == "checksum" {
					manifestChecksum = strings.TrimSpace(value)
				}
			}
		}
		if name == "cmd/main" && h.Mode&0111 == 0 {
			return errors.New("lifecycle script is not executable")
		}
		if name == "ICON.PNG" || name == "ICON_256.PNG" {
			b, err := io.ReadAll(io.LimitReader(in, maxIconBytes+1))
			if err != nil {
				return err
			}
			size := 64
			if name == "ICON_256.PNG" {
				size = 256
			}
			if err := validateIcon(b, size); err != nil {
				return err
			}
		}
		if name == "app.tgz" {
			digest := md5.New()
			tee := io.TeeReader(in, digest)
			gz, err := gzip.NewReader(tee)
			if err != nil {
				return err
			}
			app := tar.NewReader(gz)
			for {
				entry, err := app.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				p, err := clean(entry.Name)
				if err != nil {
					return err
				}
				appFound[p] = true
				if strings.Contains(p, "account.enc") || strings.Contains(p, "access.key") || strings.HasPrefix(p, "vendor/") {
					return errors.New("unexpected credentials or Python dependencies in package")
				}
				if strings.HasPrefix(p, "ui/images/icon_") && strings.HasSuffix(p, ".png") {
					size, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, "ui/images/icon_"), ".png"))
					if err != nil || size <= 0 {
						return fmt.Errorf("invalid icon name: %s", p)
					}
					b, err := io.ReadAll(io.LimitReader(app, maxIconBytes+1))
					if err != nil {
						return err
					}
					if err := validateIcon(b, size); err != nil {
						return err
					}
				}
				if p == "bin/micast-go" || p == "audio/ffmpeg" {
					if entry.Mode&0111 == 0 {
						return fmt.Errorf("%s is not executable", p)
					}
					header := make([]byte, 20)
					if _, err = io.ReadFull(app, header); err != nil {
						return err
					}
					machine := uint16(62)
					if platform == "arm" {
						machine = 183
					}
					if string(header[:4]) != "\x7fELF" || header[4] != 2 || header[5] != 1 || binary.LittleEndian.Uint16(header[18:]) != machine {
						return fmt.Errorf("wrong ELF architecture: %s", p)
					}
				}
			}
			if _, err = io.Copy(io.Discard, gz); err != nil {
				return err
			}
			gz.Close()
			if _, err = io.Copy(io.Discard, tee); err != nil {
				return err
			}
			appChecksum = hex.EncodeToString(digest.Sum(nil))
		}
	}
	if manifestChecksum == "" || manifestChecksum != appChecksum {
		return errors.New("FPK app checksum mismatch")
	}
	for _, name := range []string{"manifest", "app.tgz", "cmd/main", "cmd/install_callback", "config/privilege", "wizard/install", "ICON.PNG", "ICON_256.PNG"} {
		if !found[name] {
			return fmt.Errorf("FPK missing %s", name)
		}
	}
	for _, name := range []string{"bin/micast-go", "audio/ffmpeg", "ui/config", "ui/images/icon_64.png", "ui/images/icon_128.png", "ui/images/icon_256.png", "licenses/ffmpeg/LICENSE.txt"} {
		if !appFound[name] {
			return fmt.Errorf("app missing %s", name)
		}
	}
	return nil
}
