package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

//go:embed templates/*
var templateFS embed.FS

const (
	pkgName           = "bedrocknode-tunnel"
	binName           = "nethernet-client"
	defaultMaintainer = "Nutcrustys420 <stonercraft420@users.noreply.github.com>"
	defaultRepo       = "https://github.com/stonercraft420/BedrockNodeTunnel"
	lintianOverride   = "bedrocknode-tunnel: statically-linked-binary [usr/bin/nethernet-client]\n"
)

var (
	versionRe    = regexp.MustCompile(`^[0-9][0-9A-Za-z.]*$`)
	maintainerRe = regexp.MustCompile(`^[^<>]+ <[^<>@\s]+@[^<>\s]+>$`)
)

type config struct {
	Version    string
	OutDir     string
	Maintainer string
	URL        string
	Date       time.Time
	Targets    map[string]bool
}

type arMember struct {
	name string
	data []byte
}

type tarEntry struct {
	name string
	mode int64
	data []byte
	dir  bool
}

func main() {
	c := config{}
	var targets string
	flag.StringVar(&c.Version, "version", "1.1", "release version without a leading v")
	flag.StringVar(&c.OutDir, "out", "dist", "output directory")
	flag.StringVar(&c.Maintainer, "maintainer", envOr("BNT_MAINTAINER", defaultMaintainer), "maintainer as \"Name <email>\" (or set BNT_MAINTAINER)")
	flag.StringVar(&c.URL, "repo", defaultRepo, "project URL used in package metadata and the PKGBUILD source")
	flag.StringVar(&targets, "targets", "deb,arch", "comma-separated list of: deb, arch, windows")
	flag.Parse()

	c.URL = strings.TrimRight(c.URL, "/")
	c.Date = releaseDate()
	c.Targets = map[string]bool{}
	for _, t := range strings.Split(targets, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if t != "deb" && t != "arch" && t != "windows" {
			log.Fatalf("unknown target %q, expected deb, arch or windows", t)
		}
		c.Targets[t] = true
	}
	if len(c.Targets) == 0 {
		log.Fatal("no targets selected")
	}
	if !versionRe.MatchString(c.Version) {
		log.Fatalf("invalid -version %q", c.Version)
	}
	if !maintainerRe.MatchString(c.Maintainer) {
		log.Fatalf("invalid -maintainer %q, expected \"Name <email>\"", c.Maintainer)
	}
	for _, f := range []string{"go.mod", "client.go", "LICENSE", "packaging/bedrocknode-tunnel.service", "packaging/nethernet-client.1"} {
		if _, err := os.Stat(f); err != nil {
			log.Fatalf("run this from the repository root: %v", err)
		}
	}
	if _, err := exec.LookPath("go"); err != nil {
		log.Fatal("go toolchain not found in PATH")
	}

	if err := run(c); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func releaseDate() time.Time {
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(sec, 0).UTC()
		}
	}
	if out, err := exec.Command("git", "log", "-1", "--format=%ct").Output(); err == nil {
		if sec, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			return time.Unix(sec, 0).UTC()
		}
	}
	return time.Now().UTC()
}

func run(c config) error {
	if err := os.MkdirAll(c.OutDir, 0755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "bnt-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	var outputs []string

	if c.Targets["windows"] {
		exeName := fmt.Sprintf("%s-%s-windows-amd64.exe", binName, c.Version)
		exePath := filepath.Join(c.OutDir, exeName)
		log.Printf("building %s", exeName)
		if err := buildBinary("windows", "amd64", exePath, c.Version); err != nil {
			return err
		}
		outputs = append(outputs, exePath)
	}

	if c.Targets["deb"] {
		for _, arch := range []string{"amd64", "arm64"} {
			bin := filepath.Join(work, binName+"-"+arch)
			log.Printf("building linux/%s", arch)
			if err := buildBinary("linux", arch, bin, c.Version); err != nil {
				return err
			}
			debPath := filepath.Join(c.OutDir, fmt.Sprintf("%s_%s_%s.deb", pkgName, c.Version, arch))
			log.Printf("packaging %s", filepath.Base(debPath))
			if err := writeDeb(c, arch, bin, debPath); err != nil {
				return err
			}
			outputs = append(outputs, debPath)
		}
	}

	tarPath := filepath.Join(c.OutDir, fmt.Sprintf("%s-%s.tar.gz", pkgName, c.Version))
	if c.Targets["arch"] {
		log.Printf("packaging %s", filepath.Base(tarPath))
		if err := writeSourceTarball(c, work, tarPath); err != nil {
			return err
		}
		outputs = append(outputs, tarPath)

		tarSum, err := sha256File(tarPath)
		if err != nil {
			return err
		}
		pkgbuild := filepath.Join(c.OutDir, "PKGBUILD")
		if err := renderTemplate("PKGBUILD.tmpl", pkgbuild, map[string]string{
			"Version": c.Version, "URL": c.URL, "SHA256": tarSum,
		}); err != nil {
			return err
		}
		outputs = append(outputs, pkgbuild)
	}

	var sums strings.Builder
	for _, p := range outputs {
		s, err := sha256File(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s  %s\n", s, filepath.Base(p))
	}
	if err := os.WriteFile(filepath.Join(c.OutDir, "SHA256SUMS"), []byte(sums.String()), 0644); err != nil {
		return err
	}

	fmt.Printf("\nRelease %s written to %s/\n", c.Version, c.OutDir)
	for _, p := range append(outputs, filepath.Join(c.OutDir, "SHA256SUMS")) {
		fmt.Printf("  %s\n", filepath.Base(p))
	}
	fmt.Printf("\nUpload all of these to the GitHub release tagged v%s.\n", c.Version)
	if c.Targets["arch"] {
		fmt.Printf("Arch users: download PKGBUILD and %s into one folder, then run makepkg -si\n", filepath.Base(tarPath))
		fmt.Println("Always upload the PKGBUILD and tarball from the same run, the PKGBUILD pins the tarball checksum.")
	}
	return nil
}

func buildBinary(goos, goarch, dst, version string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", dst, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s/%s: %w", goos, goarch, err)
	}
	return nil
}

func renderTemplate(name, dst string, data any) error {
	b, err := renderBytes(name, data)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0644)
}

func renderBytes(name string, data any) ([]byte, error) {
	t, err := template.ParseFS(templateFS, "templates/"+name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeDeb(c config, arch, binPath, dst string) error {
	bin, err := os.ReadFile(binPath)
	if err != nil {
		return err
	}
	service, err := os.ReadFile("packaging/bedrocknode-tunnel.service")
	if err != nil {
		return err
	}
	man, err := os.ReadFile("packaging/nethernet-client.1")
	if err != nil {
		return err
	}
	manGz, err := gzipBytes(man)
	if err != nil {
		return err
	}
	docDir := "usr/share/doc/" + pkgName + "/"

	copyright, err := renderBytes("copyright.tmpl", c)
	if err != nil {
		return err
	}
	changelog := fmt.Sprintf("%s (%s) unstable; urgency=medium\n\n  * Release %s.\n\n -- %s  %s\n",
		pkgName, c.Version, c.Version, c.Maintainer, c.Date.Format(time.RFC1123Z))
	clGz, err := gzipBytes([]byte(changelog))
	if err != nil {
		return err
	}

	files := []tarEntry{
		{name: "usr/bin/" + binName, mode: 0755, data: bin},
		{name: "usr/lib/systemd/user/" + pkgName + ".service", mode: 0644, data: service},
		{name: docDir + "copyright", mode: 0644, data: copyright},
		{name: docDir + "changelog.gz", mode: 0644, data: clGz},
		{name: "usr/share/man/man1/" + binName + ".1.gz", mode: 0644, data: manGz},
		{name: "usr/share/lintian/overrides/" + pkgName, mode: 0644, data: []byte(lintianOverride)},
	}

	var dirs []tarEntry
	seen := map[string]bool{}
	for _, f := range files {
		parts := strings.Split(path.Dir(f.name), "/")
		for i := range parts {
			d := strings.Join(parts[:i+1], "/") + "/"
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, tarEntry{name: d, mode: 0755, dir: true})
			}
		}
	}

	data := []tarEntry{{name: "", mode: 0755, dir: true}}
	data = append(data, dirs...)
	data = append(data, files...)

	var md5s strings.Builder
	size := 0
	for _, f := range files {
		sum := md5.Sum(f.data)
		fmt.Fprintf(&md5s, "%s  %s\n", hex.EncodeToString(sum[:]), f.name)
		size += len(f.data)
	}

	control, err := renderBytes("control.tmpl", map[string]any{
		"Version": c.Version, "Arch": arch, "Maintainer": c.Maintainer, "URL": c.URL,
		"InstalledSize": (size + 1023) / 1024,
	})
	if err != nil {
		return err
	}

	ctrlTar, err := gzipTar([]tarEntry{
		{name: "", mode: 0755, dir: true},
		{name: "control", mode: 0644, data: control},
		{name: "md5sums", mode: 0644, data: []byte(md5s.String())},
	}, c.Date)
	if err != nil {
		return err
	}
	dataTar, err := gzipTar(data, c.Date)
	if err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	return writeAr(out, []arMember{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", ctrlTar},
		{"data.tar.gz", dataTar},
	}, c.Date.Unix())
}

func gzipTar(entries []tarEntry, mtime time.Time) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{
			Name:    "./" + e.name,
			Mode:    e.mode,
			ModTime: mtime,
			Uid:     0,
			Gid:     0,
			Uname:   "root",
			Gname:   "root",
		}
		if e.dir {
			h.Typeflag = tar.TypeDir
		} else {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if !e.dir {
			if _, err := tw.Write(e.data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeAr(w io.Writer, members []arMember, mtime int64) error {
	if _, err := io.WriteString(w, "!<arch>\n"); err != nil {
		return err
	}
	for _, m := range members {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, mtime, 0, 0, "100644", len(m.data))
		if len(hdr) != 60 {
			return fmt.Errorf("bad ar header for %s", m.name)
		}
		if _, err := io.WriteString(w, hdr); err != nil {
			return err
		}
		if _, err := w.Write(m.data); err != nil {
			return err
		}
		if len(m.data)%2 == 1 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeSourceTarball(c config, work, dst string) error {
	root := pkgName + "-" + c.Version
	stage := filepath.Join(work, "stage", root)
	for _, f := range []string{"client.go", "go.mod", "go.sum", "LICENSE", "packaging/bedrocknode-tunnel.service", "packaging/nethernet-client.1"} {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		target := filepath.Join(stage, f)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0644); err != nil {
			return err
		}
	}
	vendor := exec.Command("go", "mod", "vendor")
	vendor.Dir = stage
	vendor.Stdout, vendor.Stderr = os.Stdout, os.Stderr
	if err := vendor.Run(); err != nil {
		return fmt.Errorf("go mod vendor: %w", err)
	}

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	base := filepath.Dir(stage)
	err = filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		h := &tar.Header{ModTime: c.Date, Uid: 0, Gid: 0, Uname: "", Gname: ""}
		if d.IsDir() {
			h.Name, h.Typeflag, h.Mode = name+"/", tar.TypeDir, 0755
			return tw.WriteHeader(h)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h.Name, h.Typeflag, h.Mode, h.Size = name, tar.TypeReg, 0644, int64(len(b))
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0644)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
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
