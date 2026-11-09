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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"testing/fstest"
	"time"
)

// DiscoverySchema is the Agent Skills Discovery index schema this package reads.
const DiscoverySchema = "https://schemas.agentskills.io/discovery/0.2.0/schema.json"

// WellKnownIndex is where a site publishes its discovery index.
const WellKnownIndex = "/.well-known/agent-skills/index.json"

// Default limits for LoadURL.
const (
	DefaultMaxDownload  int64 = 32 << 20
	DefaultMaxExtracted int64 = 128 << 20
	DefaultMaxFiles           = 5000
	DefaultMaxSkills          = 500
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// URLOptions adjust how LoadURL fetches skills.
type URLOptions struct {
	// Client fetches every URL. The default follows redirects and times out
	// after a minute.
	Client *http.Client
	// AllowedHosts, when set, lists the only host names skills may be fetched
	// from. It applies to the index, to every skill it lists and to every
	// redirect.
	AllowedHosts []string
	// Dir is the folder inside an archive that holds the skills, for an
	// archive URL given directly. When that archive has one top-level folder,
	// as GitHub's source archives do, Dir is read inside it.
	Dir string
	// MaxDownload caps each download in bytes.
	MaxDownload int64
	// MaxExtracted caps the total size of the files inside one archive.
	MaxExtracted int64
	// MaxFiles caps the number of entries inside one archive.
	MaxFiles int
	// MaxSkills caps the number of skills one index may list.
	MaxSkills int
}

func (o URLOptions) withDefaults() URLOptions {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: time.Minute}
	}
	if o.MaxDownload <= 0 {
		o.MaxDownload = DefaultMaxDownload
	}
	if o.MaxExtracted <= 0 {
		o.MaxExtracted = DefaultMaxExtracted
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	if o.MaxSkills <= 0 {
		o.MaxSkills = DefaultMaxSkills
	}
	if len(o.AllowedHosts) > 0 {
		client := *o.Client
		previous := client.CheckRedirect
		client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
			if !o.allowed(request.URL) {
				return errorf("%s redirects to %s, which is not an allowed host", via[0].URL, request.URL.Hostname())
			}
			if previous != nil {
				return previous(request, via)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		o.Client = &client
	}
	return o
}

func (o URLOptions) allowed(address *url.URL) bool {
	if len(o.AllowedHosts) == 0 {
		return true
	}
	for _, host := range o.AllowedHosts {
		if strings.EqualFold(host, address.Hostname()) {
			return true
		}
	}
	return false
}

func isURL(location string) bool {
	return strings.HasPrefix(location, "https://") || strings.HasPrefix(location, "http://")
}

func webAddress(location string) (*url.URL, error) {
	parsed, err := url.Parse(location)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, errorf("%s is not an http or https address", location)
	}
	return parsed, nil
}

type download struct {
	data        []byte
	contentType string
	address     *url.URL
}

func fetch(ctx context.Context, address *url.URL, options URLOptions) (*download, error) {
	if !options.allowed(address) {
		return nil, errorf("%s is not an allowed host", address.Hostname())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json, text/markdown, application/gzip, application/zip;q=0.9, */*;q=0.1")
	response, err := options.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errorf("%s answered %s", address, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, options.MaxDownload+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > options.MaxDownload {
		return nil, errorf("%s is larger than %d bytes", address, options.MaxDownload)
	}
	return &download{data: data, contentType: response.Header.Get("Content-Type"), address: response.Request.URL}, nil
}

type kind int

const (
	kindUnknown kind = iota
	kindIndex
	kindSkillMD
	kindZip
	kindTarGzip
)

// detect names what a download holds: the Content-Type first, then the URL's
// extension, then the leading bytes. text/plain counts as unspecific because
// many hosts serve Markdown and JSON with it.
func detect(contentType, urlPath string, data []byte) kind {
	media, _, _ := mime.ParseMediaType(contentType)
	switch {
	case media == "application/json" || strings.HasSuffix(media, "+json"):
		return kindIndex
	case media == "text/markdown" || media == "text/x-markdown":
		return kindSkillMD
	case media == "application/zip" || media == "application/x-zip-compressed":
		return kindZip
	case media == "application/gzip" || media == "application/x-gzip" || media == "application/x-gtar" || media == "application/x-compressed-tar":
		return kindTarGzip
	}
	name := strings.ToLower(urlPath)
	switch {
	case strings.HasSuffix(name, ".json"):
		return kindIndex
	case strings.HasSuffix(name, ".md"):
		return kindSkillMD
	case strings.HasSuffix(name, ".zip"):
		return kindZip
	case strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz"):
		return kindTarGzip
	}
	lead := bytes.TrimLeft(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), " \t\r\n")
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, []byte("PK\x05\x06")):
		return kindZip
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		return kindTarGzip
	case bytes.HasPrefix(lead, []byte("{")):
		return kindIndex
	case bytes.HasPrefix(lead, []byte("---")):
		return kindSkillMD
	}
	return kindUnknown
}

// LoadURL reads skills from an http(s) URL. The URL may return an Agent
// Skills Discovery index (JSON), a single SKILL.md, or a .zip or .tar.gz
// archive. A bare site address such as https://example.com is read from its
// /.well-known/agent-skills/index.json. Everything is read in memory; nothing
// is written to disk and nothing in a skill is run.
func LoadURL(ctx context.Context, location string, options URLOptions) (*Pack, error) {
	address, err := webAddress(location)
	if err != nil {
		return nil, err
	}
	options = options.withDefaults()
	if address.Path == "" || address.Path == "/" {
		address = address.ResolveReference(&url.URL{Path: WellKnownIndex})
	}
	got, err := fetch(ctx, address, options)
	if err != nil {
		return nil, err
	}
	switch detect(got.contentType, got.address.Path, got.data) {
	case kindIndex:
		return loadIndex(ctx, got, options)
	case kindSkillMD:
		skill, err := skillFromMarkdown(got.data, got.address.String(), urlSkillFolder(got.address.Path))
		if err != nil {
			return nil, errorf("%s: %v", got.address, err)
		}
		return NewPack([]*Skill{skill})
	case kindZip, kindTarGzip:
		return loadArchiveURL(got, location, options)
	}
	return nil, errorf("%s is not a discovery index, a SKILL.md or a .zip or .tar.gz archive (Content-Type %q)", got.address, got.contentType)
}

func loadArchiveURL(got *download, location string, options URLOptions) (*Pack, error) {
	archive, err := openArchive(got, options)
	if err != nil {
		return nil, errorf("%s: %v", got.address, err)
	}
	dir := path.Join(topFolder(archive), options.Dir)
	if !fs.ValidPath(dir) {
		return nil, errorf("%s is not a folder inside %s", options.Dir, location)
	}
	pack, err := LoadFS(archive, dir)
	if err != nil {
		return nil, err
	}
	for _, skill := range pack.order {
		skill.Root = location + "!/" + skill.dir
		if skill.dir == "." {
			skill.Root = location
			skill.folder = archiveName(got.address.Path)
		}
	}
	return pack, nil
}

type indexEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Digest      string `json:"digest"`
}

func loadIndex(ctx context.Context, got *download, options URLOptions) (*Pack, error) {
	var index struct {
		Schema *string       `json:"$schema"`
		Skills *[]indexEntry `json:"skills"`
	}
	if err := json.Unmarshal(got.data, &index); err != nil {
		return nil, errorf("%s is not a discovery index: %v", got.address, err)
	}
	switch {
	case index.Schema == nil:
		return nil, errorf("%s has no $schema, so it is a v0.1.0 discovery index; skillpack reads %s, whose digests let it verify each skill", got.address, DiscoverySchema)
	case *index.Schema != DiscoverySchema:
		return nil, errorf("%s uses discovery schema %s; skillpack reads %s", got.address, *index.Schema, DiscoverySchema)
	case index.Skills == nil:
		return nil, errorf("%s has no skills list", got.address)
	case len(*index.Skills) > options.MaxSkills:
		return nil, errorf("%s lists more than %d skills", got.address, options.MaxSkills)
	}
	skills := []*Skill{}
	warnings := []string{}
	for position, entry := range *index.Skills {
		label := fmt.Sprintf("%s skill %d (%s)", got.address, position+1, entry.Name)
		if entry.Type != "skill-md" && entry.Type != "archive" {
			warnings = append(warnings, fmt.Sprintf("%s: skipped type %q, which this version does not read", label, entry.Type))
			continue
		}
		skill, err := loadIndexEntry(ctx, got.address, entry, options)
		if err != nil {
			return nil, errorf("%s: %v", label, err)
		}
		skills = append(skills, skill)
	}
	pack, err := NewPack(skills)
	if err != nil {
		return nil, err
	}
	pack.warnings = warnings
	return pack, nil
}

func loadIndexEntry(ctx context.Context, base *url.URL, entry indexEntry, options URLOptions) (*Skill, error) {
	switch {
	case len(entry.Name) == 0 || len(entry.Name) > 64 || !namePattern.MatchString(entry.Name):
		return nil, errorf("name must be 1 to 64 lowercase letters, digits and single hyphens")
	case strings.TrimSpace(entry.Description) == "" || len([]rune(entry.Description)) > 1024:
		return nil, errorf("description must be 1 to 1024 characters")
	case !digestPattern.MatchString(entry.Digest):
		return nil, errorf("digest must be sha256: followed by 64 lowercase hex characters")
	case entry.URL == "":
		return nil, errorf("url is missing")
	}
	reference, err := url.Parse(entry.URL)
	if err != nil {
		return nil, errorf("url %q is not a URL", entry.URL)
	}
	address, err := webAddress(base.ResolveReference(reference).String())
	if err != nil {
		return nil, err
	}
	got, err := fetch(ctx, address, options)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(got.data)
	if actual := "sha256:" + hex.EncodeToString(sum[:]); actual != entry.Digest {
		return nil, errorf("%s has digest %s, not the %s the index lists", address, actual, entry.Digest)
	}
	var skill *Skill
	if entry.Type == "skill-md" {
		skill, err = skillFromMarkdown(got.data, address.String(), entry.Name)
	} else {
		var archive fs.FS
		if archive, err = openArchive(got, options); err == nil {
			if skill, err = LoadSkillFS(archive, "."); err == nil {
				skill.Root = address.String()
				skill.folder = entry.Name
			}
		}
	}
	if err != nil {
		return nil, errorf("%s: %v", address, err)
	}
	skill.Digest = entry.Digest
	skill.indexDescription = entry.Description
	return skill, nil
}

func skillFromMarkdown(data []byte, root, folder string) (*Skill, error) {
	skill, err := readSkill(fstest.MapFS{"SKILL.md": {Data: data, Mode: 0o644}}, ".", root, folder)
	if err != nil {
		return nil, err
	}
	return skill, nil
}

// urlSkillFolder is the folder name a SKILL.md at a URL belongs to: the
// parent folder for .../name/SKILL.md, or the file name for .../name.md.
func urlSkillFolder(urlPath string) string {
	base := path.Base(urlPath)
	if strings.EqualFold(base, "SKILL.md") {
		return path.Base(path.Dir(urlPath))
	}
	return strings.TrimSuffix(base, path.Ext(base))
}

// topFolder is the archive's one top-level folder when it has nothing else
// at its root, and "." otherwise.
func topFolder(archive fs.FS) string {
	entries, err := fs.ReadDir(archive, ".")
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return "."
	}
	return entries[0].Name()
}

func archiveName(urlPath string) string {
	name := path.Base(urlPath)
	for _, suffix := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// archiveBuilder collects an archive's regular files in memory under the
// size and entry limits, refusing unsafe paths and links out of the archive.
type archiveBuilder struct {
	files   fstest.MapFS
	options URLOptions
	total   int64
	count   int
}

func (b *archiveBuilder) name(raw string) (string, error) {
	b.count++
	if b.count > b.options.MaxFiles {
		return "", errorf("the archive holds more than %d entries", b.options.MaxFiles)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(raw, "./"), "/")
	if name == "" || name == "." {
		return "", nil
	}
	if strings.Contains(raw, "\\") || !fs.ValidPath(name) {
		return "", errorf("the archive holds an unsafe path: %s", raw)
	}
	return name, nil
}

func (b *archiveBuilder) link(name, target string, hard bool) error {
	resolved := path.Clean(target)
	if !hard {
		resolved = path.Join(path.Dir(name), target)
	}
	if strings.HasPrefix(target, "/") || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return errorf("the archive links %s to %s, outside the archive", name, target)
	}
	return nil
}

func (b *archiveBuilder) file(name string, size int64, content io.Reader) error {
	if size < 0 || b.total+size > b.options.MaxExtracted {
		return errorf("the archive unpacks to more than %d bytes", b.options.MaxExtracted)
	}
	data, err := io.ReadAll(io.LimitReader(content, size+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errorf("the archive entry %s is not the size it declares", name)
	}
	b.total += size
	b.files[name] = &fstest.MapFile{Data: data, Mode: 0o644}
	return nil
}

func openArchive(got *download, options URLOptions) (fs.FS, error) {
	builder := &archiveBuilder{files: fstest.MapFS{}, options: options}
	switch detect(got.contentType, got.address.Path, got.data) {
	case kindZip:
		return builder.files, builder.readZip(got.data)
	case kindTarGzip:
		return builder.files, builder.readTarGzip(got.data)
	}
	return nil, errorf("it is not a .zip or .tar.gz archive")
}

func (b *archiveBuilder) readZip(data []byte) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, entry := range reader.File {
		name, err := b.name(entry.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		mode := entry.Mode()
		switch {
		case mode.IsDir():
			b.files[name] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		case mode&fs.ModeSymlink != 0:
			target, err := readZipEntry(entry, 4096)
			if err != nil {
				return err
			}
			if err := b.link(name, string(target), false); err != nil {
				return err
			}
		case mode.IsRegular():
			if entry.UncompressedSize64 > uint64(b.options.MaxExtracted) {
				return errorf("the archive unpacks to more than %d bytes", b.options.MaxExtracted)
			}
			content, err := entry.Open()
			if err != nil {
				return err
			}
			err = b.file(name, int64(entry.UncompressedSize64), content)
			content.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func readZipEntry(entry *zip.File, limit int64) ([]byte, error) {
	content, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer content.Close()
	return io.ReadAll(io.LimitReader(content, limit))
}

func (b *archiveBuilder) readTarGzip(data []byte) error {
	unzipped, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer unzipped.Close()
	reader := tar.NewReader(unzipped)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := b.name(header.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			b.files[name] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		case tar.TypeSymlink, tar.TypeLink:
			if err := b.link(name, header.Linkname, header.Typeflag == tar.TypeLink); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := b.file(name, header.Size, reader); err != nil {
				return err
			}
		}
	}
}
