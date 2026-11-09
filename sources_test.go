package skillpack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const layoutSkill = "---\nname: page-layout\ndescription: Lay out a page.\n---\n\nSee [grids](references/grids.md).\n"

func TestLoadFSReadsSkillsFromAnyFileSystem(t *testing.T) {
	files := fstest.MapFS{
		"skills/page-layout/SKILL.md":            {Data: []byte(layoutSkill)},
		"skills/page-layout/references/grids.md": {Data: []byte("# Grids\n")},
		"skills/notes.txt":                       {Data: []byte("not a skill")},
	}
	pack, err := LoadFS(files, "skills")
	if err != nil {
		t.Fatal(err)
	}
	skill, err := pack.Get("page-layout")
	if err != nil || !skill.Valid() || skill.Root != "skills/page-layout" {
		t.Fatalf("skill %#v: %v, %#v", skill, err, skill.Validate())
	}
	if text, err := skill.Reference("grids"); err != nil || text != "# Grids\n" {
		t.Fatalf("reference = %q, %v", text, err)
	}
	if _, err := skill.Reference("../../notes.txt"); err == nil {
		t.Fatal("read a file outside the skill")
	}
	installed, err := pack.Install(t.TempDir())
	if err != nil || len(installed) != 1 {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installed[0], "references", "grids.md")); err != nil {
		t.Fatalf("installed copy lacks its reference: %v", err)
	}
	if _, err := LoadFS(files, "../outside"); err == nil {
		t.Fatal("accepted a folder outside the file system")
	}
}

func TestASymlinkOutOfASkillCannotBeRead(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(base, "secret.md")
	root := filepath.Join(base, "page-layout")
	_ = os.MkdirAll(filepath.Join(root, "references"), 0o755)
	_ = os.WriteFile(secret, []byte("private"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(layoutSkill), 0o644)
	if err := os.Symlink(secret, filepath.Join(root, "references", "grids.md")); err != nil {
		t.Skip("symbolic links are not available:", err)
	}
	skill, err := LoadSkill(root)
	if err != nil {
		t.Fatal(err)
	}
	if text, err := skill.Reference("grids"); err == nil {
		t.Fatalf("read %q through a link out of the skill", text)
	}
	if got := codes(skill.Validate()); len(got) != 1 || got[0] != "link-outside" {
		t.Fatalf("codes = %#v", got)
	}
	checks := filepath.Join(base, "checks.yaml")
	_ = os.WriteFile(checks, []byte("checks:\n  - id: leaked\n"), 0o644)
	if err := os.Symlink(checks, filepath.Join(root, "checks.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := skill.Checks(); got != nil {
		t.Fatalf("read checks %#v through a link out of the skill", got)
	}
}

func zipped(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, text := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte(text))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func tarred(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for name, text := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(text)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(text))
	}
	_ = writer.Close()
	_ = compressed.Close()
	return buffer.Bytes()
}

func serve(t *testing.T, routes map[string][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLoadURLReadsAGitHubStyleZip(t *testing.T) {
	server := serve(t, map[string][]byte{"/archive/main.zip": zipped(t, map[string]string{
		"repo-main/skills/page-layout/SKILL.md":            layoutSkill,
		"repo-main/skills/page-layout/references/grids.md": "# Grids\n",
		"repo-main/README.md":                              "readme",
	})})
	pack, err := LoadURL(context.Background(), server.URL+"/archive/main.zip", URLOptions{Dir: "skills"})
	if err != nil {
		t.Fatal(err)
	}
	skill, err := pack.Get("page-layout")
	if err != nil || !skill.Valid() || len(skill.References()) != 1 {
		t.Fatalf("skill: %v, %#v", err, skill.Validate())
	}
	if skill.Root != server.URL+"/archive/main.zip!/repo-main/skills/page-layout" {
		t.Fatalf("root = %q", skill.Root)
	}
	if loaded, err := Load(server.URL + "/archive/main.zip"); err != nil || loaded.Len() != 0 {
		t.Fatalf("Load without the folder found %d skills: %v", loaded.Len(), err)
	}
}

func TestLoadURLNamesASingleSkillArchiveByItsFile(t *testing.T) {
	server := serve(t, map[string][]byte{"/page-layout.tar.gz": tarred(t, map[string]string{
		"SKILL.md":            layoutSkill,
		"references/grids.md": "# Grids\n",
	})})
	pack, err := Load(server.URL + "/page-layout.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := pack.Get("page-layout")
	if skill == nil || !skill.Valid() || skill.Folder() != "page-layout" {
		t.Fatalf("skill %#v", skill)
	}
}

func TestLoadURLRefusesWhatItCannotTrust(t *testing.T) {
	big := zipped(t, map[string]string{"repo/page-layout/SKILL.md": layoutSkill + strings.Repeat("x", 4096)})
	server := serve(t, map[string][]byte{"/big.zip": big, "/page.html": []byte("<html>")})
	ctx := context.Background()
	cases := map[string]struct {
		location string
		options  URLOptions
		want     string
	}{
		"download too large": {server.URL + "/big.zip", URLOptions{MaxDownload: 100}, "larger than 100 bytes"},
		"unpacks too large":  {server.URL + "/big.zip", URLOptions{MaxExtracted: 100}, "unpacks to more than 100 bytes"},
		"not a skill":        {server.URL + "/page.html", URLOptions{}, "is not a discovery index, a SKILL.md or a .zip or .tar.gz archive"},
		"missing":            {server.URL + "/gone.zip", URLOptions{}, "404"},
		"not http":           {"file:///etc/passwd", URLOptions{}, "not an http or https address"},
		"folder escapes":     {server.URL + "/big.zip", URLOptions{Dir: "../.."}, "is not a folder inside"},
	}
	for name, test := range cases {
		if _, err := LoadURL(ctx, test.location, test.options); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
