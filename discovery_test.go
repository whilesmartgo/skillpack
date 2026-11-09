package skillpack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const reviewSkill = "---\nname: code-review\ndescription: Review code for bugs.\n---\n\n# Code review\n"

type served struct {
	contentType string
	body        []byte
	redirect    string
}

func site(t *testing.T, routes map[string]served) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.URL.Path]
		switch {
		case !ok:
			http.NotFound(w, r)
		case route.redirect != "":
			http.Redirect(w, r, route.redirect, http.StatusFound)
		default:
			if route.contentType != "" {
				w.Header().Set("Content-Type", route.contentType)
			}
			_, _ = w.Write(route.body)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func index(t *testing.T, schema any, skills ...map[string]string) []byte {
	t.Helper()
	document := map[string]any{"skills": skills}
	if schema != nil {
		document["$schema"] = schema
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type link struct{ name, target string }

func tarWith(t *testing.T, files map[string]string, links ...link) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for name, text := range files {
		_ = writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(text)), Typeflag: tar.TypeReg})
		_, _ = writer.Write([]byte(text))
	}
	for _, item := range links {
		_ = writer.WriteHeader(&tar.Header{Name: item.name, Linkname: item.target, Typeflag: tar.TypeSymlink})
	}
	_ = writer.Close()
	_ = compressed.Close()
	return buffer.Bytes()
}

func TestAnIndexLoadsEachSkillItListsAfterCheckingItsDigest(t *testing.T) {
	archive := tarWith(t, map[string]string{"SKILL.md": layoutSkill, "references/grids.md": "# Grids\n"})
	markdown := []byte(reviewSkill)
	routes := map[string]served{
		"/.well-known/agent-skills/code-review/SKILL.md": {"text/markdown", markdown, ""},
		"/cdn/page-layout.tar.gz":                        {"application/gzip", archive, ""},
	}
	routes[WellKnownIndex] = served{"application/json", index(t, DiscoverySchema,
		map[string]string{"name": "code-review", "type": "skill-md", "description": "Review code for bugs.", "url": "code-review/SKILL.md", "digest": digest(markdown)},
		map[string]string{"name": "page-layout", "type": "archive", "description": "Lay out a page.", "url": "/cdn/page-layout.tar.gz", "digest": digest(archive)},
		map[string]string{"name": "later", "type": "plugin", "description": "A future type.", "url": "later.bin", "digest": digest(nil)},
	), ""}
	server := site(t, routes)

	pack, err := LoadURL(context.Background(), server.URL, URLOptions{AllowedHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := pack.Index(); !reflect.DeepEqual(got, []Entry{{Name: "code-review", Description: "Review code for bugs."}, {Name: "page-layout", Description: "Lay out a page."}}) {
		t.Fatalf("index = %#v", got)
	}
	for _, skill := range pack.Skills() {
		if problems := skill.Validate(); len(problems) != 0 {
			t.Fatalf("%s: %#v", skill.Name(), problems)
		}
	}
	layout, _ := pack.Get("page-layout")
	if layout.Digest != digest(archive) || layout.Root != server.URL+"/cdn/page-layout.tar.gz" {
		t.Fatalf("layout digest %q root %q", layout.Digest, layout.Root)
	}
	if text, err := layout.Reference("grids"); err != nil || text != "# Grids\n" {
		t.Fatalf("reference = %q, %v", text, err)
	}
	if warnings := pack.Warnings(); len(warnings) != 1 || !strings.Contains(warnings[0], `skipped type "plugin"`) {
		t.Fatalf("warnings = %#v", warnings)
	}
	if loaded, err := Load(server.URL + WellKnownIndex); err != nil || loaded.Len() != 2 || len(loaded.Warnings()) != 1 {
		t.Fatalf("Load kept %d skills and %d warnings: %v", loaded.Len(), len(loaded.Warnings()), err)
	}
}

func TestAnIndexIsRefusedWhenItCannotBeTrusted(t *testing.T) {
	markdown := []byte(reviewSkill)
	entry := func(change map[string]string) map[string]string {
		base := map[string]string{"name": "code-review", "type": "skill-md", "description": "Review code for bugs.", "url": "/SKILL.md", "digest": digest(markdown)}
		for key, value := range change {
			base[key] = value
		}
		return base
	}
	cases := map[string]struct {
		index []byte
		want  string
	}{
		"v0.1.0, no schema":   {index(t, nil, entry(nil)), "is a v0.1.0 discovery index"},
		"unknown schema":      {index(t, "https://schemas.agentskills.io/discovery/9.0.0/schema.json", entry(nil)), "uses discovery schema"},
		"no skills list":      {[]byte(`{"$schema":"` + DiscoverySchema + `"}`), "has no skills list"},
		"digest mismatch":     {index(t, DiscoverySchema, entry(map[string]string{"digest": digest([]byte("other"))})), "not the sha256:"},
		"malformed digest":    {index(t, DiscoverySchema, entry(map[string]string{"digest": "sha256:ABC"})), "digest must be sha256:"},
		"invalid name":        {index(t, DiscoverySchema, entry(map[string]string{"name": "Code_Review"})), "name must be"},
		"missing description": {index(t, DiscoverySchema, entry(map[string]string{"description": ""})), "description must be"},
		"non-web url":         {index(t, DiscoverySchema, entry(map[string]string{"url": "file:///etc/passwd"})), "not an http or https address"},
		"other host":          {index(t, DiscoverySchema, entry(map[string]string{"url": "http://localhost:1/SKILL.md"})), "localhost is not an allowed host"},
		"not JSON":            {[]byte("{nope"), "is not a discovery index"},
	}
	for name, test := range cases {
		server := site(t, map[string]served{"/index.json": {"application/json", test.index, ""}, "/SKILL.md": {"text/markdown", markdown, ""}})
		_, err := LoadURL(context.Background(), server.URL+"/index.json", URLOptions{AllowedHosts: []string{"127.0.0.1"}})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAnIndexDescriptionThatDiffersIsAWarning(t *testing.T) {
	markdown := []byte(reviewSkill)
	server := site(t, map[string]served{
		"/index.json": {"application/json", index(t, DiscoverySchema, map[string]string{"name": "code-review", "type": "skill-md", "description": "Something else.", "url": "/SKILL.md", "digest": digest(markdown)}), ""},
		"/SKILL.md":   {"text/markdown", markdown, ""},
	})
	pack, err := LoadURL(context.Background(), server.URL+"/index.json", URLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := pack.Get("code-review")
	if got := codes(skill.Validate()); !reflect.DeepEqual(got, []string{"index-description"}) || !skill.Valid() {
		t.Fatalf("codes = %#v", got)
	}
}

func TestAnIndexNameMustMatchTheSkill(t *testing.T) {
	markdown := []byte(reviewSkill)
	server := site(t, map[string]served{
		"/index.json": {"application/json", index(t, DiscoverySchema, map[string]string{"name": "reviewer", "type": "skill-md", "description": "Review code for bugs.", "url": "/SKILL.md", "digest": digest(markdown)}), ""},
		"/SKILL.md":   {"text/markdown", markdown, ""},
	})
	pack, err := LoadURL(context.Background(), server.URL+"/index.json", URLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(pack.Problems()["code-review"]); !reflect.DeepEqual(got, []string{"name-folder"}) {
		t.Fatalf("codes = %#v", got)
	}
}

func TestALinkIsReadByWhatItReturns(t *testing.T) {
	markdown := []byte(reviewSkill)
	plainIndex := index(t, DiscoverySchema, map[string]string{"name": "code-review", "type": "skill-md", "description": "Review code for bugs.", "url": "code-review/SKILL.md", "digest": digest(markdown)})
	server := site(t, map[string]served{
		"/skills/code-review/SKILL.md": {"text/plain; charset=utf-8", markdown, ""},
		"/skills/index.json":           {"text/plain; charset=utf-8", plainIndex, ""},
		"/download":                    {"application/octet-stream", markdown, ""},
		"/moved":                       {"", nil, "/skills/code-review/SKILL.md"},
	})
	for location, folder := range map[string]string{
		"/skills/code-review/SKILL.md": "code-review",
		"/skills/index.json":           "code-review",
		"/moved":                       "code-review",
		"/download":                    "download",
	} {
		pack, err := LoadURL(context.Background(), server.URL+location, URLOptions{})
		if err != nil {
			t.Fatalf("%s: %v", location, err)
		}
		skill, err := pack.Get("code-review")
		if err != nil || skill.Folder() != folder || skill.Body != "\n# Code review\n" {
			t.Fatalf("%s: folder %q, %v", location, skill.Folder(), err)
		}
	}
}

func TestRedirectsMustStayOnAllowedHosts(t *testing.T) {
	server := site(t, map[string]served{"/SKILL.md": {"text/markdown", []byte(reviewSkill), ""}})
	away := site(t, map[string]served{"/hop": {"", nil, strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/SKILL.md"}})
	_, err := LoadURL(context.Background(), away.URL+"/hop", URLOptions{AllowedHosts: []string{"127.0.0.1"}})
	if err == nil || !strings.Contains(err.Error(), "which is not an allowed host") {
		t.Fatalf("redirect off the allowlist: %v", err)
	}
}

func TestArchivesWithUnsafeEntriesAreRefused(t *testing.T) {
	zipLink := func() []byte {
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		entry, _ := writer.Create("SKILL.md")
		_, _ = entry.Write([]byte(layoutSkill))
		header := &zip.FileHeader{Name: "references/grids.md"}
		header.SetMode(0o777 | 1<<27)
		linked, _ := writer.CreateHeader(header)
		_, _ = linked.Write([]byte("../../secret"))
		_ = writer.Close()
		return buffer.Bytes()
	}
	cases := map[string]struct {
		archive     []byte
		contentType string
		want        string
	}{
		"tar symlink out":   {tarWith(t, map[string]string{"SKILL.md": layoutSkill}, link{"references/grids.md", "../../secret"}), "application/gzip", "outside the archive"},
		"tar absolute link": {tarWith(t, map[string]string{"SKILL.md": layoutSkill}, link{"references/grids.md", "/etc/passwd"}), "application/gzip", "outside the archive"},
		"tar traversal":     {tarWith(t, map[string]string{"SKILL.md": layoutSkill, "../escape.md": "x"}), "application/gzip", "unsafe path"},
		"zip symlink out":   {zipLink(), "application/zip", "outside the archive"},
		"zip traversal":     {zipped(t, map[string]string{"SKILL.md": layoutSkill, "../escape.md": "x"}), "application/zip", "unsafe path"},
	}
	for name, test := range cases {
		server := site(t, map[string]served{
			"/index.json":   {"application/json", index(t, DiscoverySchema, map[string]string{"name": "page-layout", "type": "archive", "description": "Lay out a page.", "url": "/skill", "digest": digest(test.archive)}), ""},
			"/skill":        {test.contentType, test.archive, ""},
			"/direct.bytes": {test.contentType, test.archive, ""},
		})
		for _, location := range []string{"/index.json", "/direct.bytes"} {
			if _, err := LoadURL(context.Background(), server.URL+location, URLOptions{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("%s via %s: %v", name, location, err)
			}
		}
	}
	inside := tarWith(t, map[string]string{"SKILL.md": layoutSkill, "references/grids.md": "# Grids\n"}, link{"references/copy.md", "grids.md"})
	server := site(t, map[string]served{"/page-layout.tar.gz": {"application/gzip", inside, ""}})
	pack, err := LoadURL(context.Background(), server.URL+"/page-layout.tar.gz", URLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := pack.Get("page-layout")
	if got := skill.References(); !reflect.DeepEqual(got, []string{"grids.md"}) {
		t.Fatalf("a link inside the archive should be left out, references = %#v", got)
	}
}

func TestAnIndexArchiveMustHoldSKILLmdAtItsRoot(t *testing.T) {
	wrapped := tarWith(t, map[string]string{"page-layout/SKILL.md": layoutSkill})
	server := site(t, map[string]served{
		"/index.json":         {"application/json", index(t, DiscoverySchema, map[string]string{"name": "page-layout", "type": "archive", "description": "Lay out a page.", "url": "/page-layout.tar.gz", "digest": digest(wrapped)}), ""},
		"/page-layout.tar.gz": {"application/gzip", wrapped, ""},
	})
	if _, err := LoadURL(context.Background(), server.URL+"/index.json", URLOptions{}); err == nil || !strings.Contains(err.Error(), "has no SKILL.md") {
		t.Fatalf("wrapped archive in an index: %v", err)
	}
}

func TestAnIndexListingTooManySkillsIsRefused(t *testing.T) {
	entries := []map[string]string{}
	for range 3 {
		entries = append(entries, map[string]string{"name": "a", "type": "skill-md", "description": "x", "url": "/a", "digest": digest(nil)})
	}
	server := site(t, map[string]served{"/index.json": {"application/json", index(t, DiscoverySchema, entries...), ""}})
	if _, err := LoadURL(context.Background(), server.URL+"/index.json", URLOptions{MaxSkills: 2}); err == nil || !strings.Contains(err.Error(), "more than 2 skills") {
		t.Fatalf("too many skills: %v", err)
	}
}
