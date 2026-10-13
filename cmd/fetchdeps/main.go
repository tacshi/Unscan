// Fetchdeps downloads what Unscan needs at run time into resources/:
// the PP-OCRv6 small detection and recognition models with the
// recognizer's character dictionary, the fonts that text PDFs are set in,
// and the ONNX Runtime library of each platform asked for. Every file is
// pinned and checked against its SHA-256.
//
//	go run ./cmd/fetchdeps                       # the models and this machine's runtime
//	go run ./cmd/fetchdeps -platforms all        # every platform ONNX Runtime ships
//	go run ./cmd/fetchdeps -platforms linux-amd64,windows-amd64
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// A remote file and its SHA-256.
type remote struct {
	url    string
	sha256 string
}

const (
	detRevision = "28fe5895c24fd108c19eb3e8479f4ab385fbfc62"
	recRevision = "b8f84f0b80c529de40b4fbb3544b84fa7233a513"
	hf          = "https://huggingface.co/PaddlePaddle/"
)

var (
	detModel = remote{hf + "PP-OCRv6_small_det_onnx/resolve/" + detRevision + "/inference.onnx",
		"d73e0058b7a8086bbd57f3d10b8bcd4ff95363f67e06e2762b5e814fe9c9410e"}
	recModel = remote{hf + "PP-OCRv6_small_rec_onnx/resolve/" + recRevision + "/inference.onnx",
		"5435fd747c9e0efe15a96d0b378d5bd157e9492ed8fd80edf08f30d02fa24634"}
	recConfig = remote{hf + "PP-OCRv6_small_rec_onnx/resolve/" + recRevision + "/inference.yml",
		"ab078671bb49f06228eadccd34f1bb501e157f7a047095ffb943ba81512c77d1"}

	// Noto Serif SC and Noto Sans SC, Regular and Bold (SIL Open Font
	// License 1.1), TrueType builds from Google Fonts, and their licenses,
	// which go with them, from the google/fonts repository.
	textFonts = []struct {
		r    remote
		name string
	}{
		{remote{"https://fonts.gstatic.com/s/notoserifsc/v36/H4cyBXePl9DZ0Xe7gG9cyOj7uK2-n-D2rd4FY7SCqyWv847hdDWC.ttf",
			"c358a69de3df5f423a8946a7b03e3126fb6ba116f1130a463455b4e39e1f0742"}, "NotoSerifSC-Regular.ttf"},
		{remote{"https://fonts.gstatic.com/s/notoserifsc/v36/H4cyBXePl9DZ0Xe7gG9cyOj7uK2-n-D2rd4FY7RlrCWv847hdDWC.ttf",
			"9c6a2ba736e4b499c84f65ce366bd3625c0401885fd3e585ace50fb738b96ff6"}, "NotoSerifSC-Bold.ttf"},
		{remote{"https://fonts.gstatic.com/s/notosanssc/v41/k3kCo84MPvpLmixcA63oeAL7Iqp5IZJF9bmaG9_FnYxNbPzS5HE.ttf",
			"d45f67f0a7c0ca3f256950777ce6a61cc7ce5f9696d02900cbbaac25f8aa7d16"}, "NotoSansSC-Regular.ttf"},
		{remote{"https://fonts.gstatic.com/s/notosanssc/v41/k3kCo84MPvpLmixcA63oeAL7Iqp5IZJF9bmaGzjCnYxNbPzS5HE.ttf",
			"9a38ae0ab28cd5a256f9ea8e00dedc688aac17f7915fcb000572990afa956b96"}, "NotoSansSC-Bold.ttf"},
		{remote{"https://raw.githubusercontent.com/google/fonts/8b0a1d0f5983c89bc2b93f1b5fb55f9e252744b5/ofl/notoserifsc/OFL.txt",
			"5e0da210fb04058a8c0087985d2d456b931c2579811a49655721d3cf0c36b6d6"}, "NotoSerifSC-OFL.txt"},
		{remote{"https://raw.githubusercontent.com/google/fonts/a85815a42757630ce188fdad368c2dfc444d4773/ofl/notosanssc/OFL.txt",
			"1c05c68c34f9708415aada51f17e1b0092d2cea709bf4a94cd38114f9e73d7d9"}, "NotoSansSC-OFL.txt"},
	}
)

// ONNX Runtime 1.27.1, the version onnxruntime_purego is built against.
// Microsoft ships no build for Intel Macs.
const ortVersion = "1.27.1"

type runtimeBuild struct {
	archive remote
	member  string // the library inside the archive
	name    string // its name among the app's resources
}

var runtimes = map[string]runtimeBuild{
	"darwin-arm64": {
		remote{ortURL("osx-arm64", "tgz"), "e42b77a7281cc6e55141bf44fcfbac2c782b823a491bbb6ac33c781dd991f8a6"},
		"lib/libonnxruntime." + ortVersion + ".dylib", "libonnxruntime.dylib",
	},
	"linux-amd64": {
		remote{ortURL("linux-x64", "tgz"), "25b1ef1fea1acd210d63f8f24dc870ad6e077795ce1f54876252c6d3803c15af"},
		"lib/libonnxruntime.so." + ortVersion, "libonnxruntime.so",
	},
	"linux-arm64": {
		remote{ortURL("linux-aarch64", "tgz"), "33c67e33d1e25b816878366ea276589a024f71f000e7ff955c4b33224d639edd"},
		"lib/libonnxruntime.so." + ortVersion, "libonnxruntime.so",
	},
	"windows-amd64": {
		remote{ortURL("win-x64", "zip"), "2e00414a63fdef0914cd5a5ede6c707844878e0c08e1b6693842f0451b2df2a1"},
		"lib/onnxruntime.dll", "onnxruntime.dll",
	},
	"windows-arm64": {
		remote{ortURL("win-arm64", "zip"), "6e22c2061ba6400b42a59663d700c8694e4e8fe654cf452c4700c24237407ae1"},
		"lib/onnxruntime.dll", "onnxruntime.dll",
	},
}

func ortURL(platform, ext string) string {
	return fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-%s-%s.%s",
		ortVersion, platform, ortVersion, ext)
}

func main() {
	log.SetFlags(0)
	dir := flag.String("dir", "resources", "the app's resources directory")
	platforms := flag.String("platforms", runtime.GOOS+"-"+runtime.GOARCH,
		`ONNX Runtime platforms, comma-separated, or "all"`)
	flag.Parse()

	models := filepath.Join(*dir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, f := range []struct {
		r    remote
		name string
	}{{detModel, "det.onnx"}, {recModel, "rec.onnx"}} {
		if err := fetchFile(f.r, filepath.Join(models, f.name)); err != nil {
			log.Fatal(err)
		}
	}
	if err := fetchDict(filepath.Join(models, "rec_dict.txt")); err != nil {
		log.Fatal(err)
	}
	fonts := filepath.Join(*dir, "fonts")
	for _, f := range textFonts {
		if err := fetchFile(f.r, filepath.Join(fonts, f.name)); err != nil {
			log.Fatal(err)
		}
	}

	var names []string
	if *platforms == "all" {
		for p := range runtimes {
			names = append(names, p)
		}
		slices.Sort(names)
	} else {
		names = strings.Split(*platforms, ",")
	}
	for _, p := range names {
		b, ok := runtimes[p]
		if !ok {
			log.Fatalf("no ONNX Runtime %s build for %s", ortVersion, p)
		}
		if err := fetchRuntime(b, filepath.Join(*dir, p, b.name)); err != nil {
			log.Fatal(err)
		}
	}
}

// fetchFile downloads r to dst unless dst already has its contents.
func fetchFile(r remote, dst string) error {
	if sum, err := fileSHA256(dst); err == nil && sum == r.sha256 {
		log.Printf("ok       %s", dst)
		return nil
	}
	data, err := download(r)
	if err != nil {
		return err
	}
	log.Printf("fetched  %s (%.1f MB)", dst, float64(len(data))/1e6)
	return writeFile(dst, data)
}

// fetchDict writes the recognizer's characters, one per line, from the
// character_dict list of its inference.yml.
func fetchDict(dst string) error {
	if _, err := os.Stat(dst); err == nil {
		log.Printf("ok       %s", dst)
		return nil
	}
	data, err := download(recConfig)
	if err != nil {
		return err
	}
	var chars []string
	in := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "character_dict:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		item, ok := strings.CutPrefix(line, "  - ")
		if !ok {
			break
		}
		// Items are plain scalars or single-quoted ones, where '' is a quote.
		if len(item) >= 2 && item[0] == '\'' && item[len(item)-1] == '\'' {
			item = strings.ReplaceAll(item[1:len(item)-1], "''", "'")
		}
		chars = append(chars, item)
	}
	if len(chars) == 0 {
		return errors.New("inference.yml has no character_dict")
	}
	log.Printf("wrote    %s (%d characters)", dst, len(chars))
	return writeFile(dst, []byte(strings.Join(chars, "\n")+"\n"))
}

// fetchRuntime extracts the library of an ONNX Runtime archive to dst.
func fetchRuntime(b runtimeBuild, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		log.Printf("ok       %s", dst)
		return nil
	}
	data, err := download(b.archive)
	if err != nil {
		return err
	}
	var lib []byte
	if strings.HasSuffix(b.archive.url, ".zip") {
		lib, err = fromZip(data, b.member)
	} else {
		lib, err = fromTarGz(data, b.member)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path.Base(b.archive.url), err)
	}
	log.Printf("fetched  %s (%.1f MB)", dst, float64(len(lib))/1e6)
	return writeFile(dst, lib)
}

// Archives hold their files under a top-level directory named after them.
func memberMatches(name, member string) bool {
	_, rest, ok := strings.Cut(name, "/")
	return ok && rest == member
}

func fromTarGz(data []byte, member string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("no %s: %w", member, err)
		}
		if h.Typeflag == tar.TypeReg && memberMatches(h.Name, member) {
			return io.ReadAll(tr)
		}
	}
}

func fromZip(data []byte, member string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if memberMatches(f.Name, member) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("no %s", member)
}

func download(r remote) ([]byte, error) {
	log.Printf("download %s", r.url)
	resp, err := http.Get(r.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", r.url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != r.sha256 {
		return nil, fmt.Errorf("%s: SHA-256 %s, want %s", r.url, got, r.sha256)
	}
	return data, nil
}

func fileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFile(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
