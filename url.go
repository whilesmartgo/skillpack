package skillpack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"testing/fstest"
	"time"
)

// Default limits for LoadURL.
const (
	DefaultMaxDownload  int64 = 32 << 20
	DefaultMaxExtracted int64 = 128 << 20
	DefaultMaxFiles           = 5000
)

// URLOptions adjust how LoadURL fetches and opens an archive.
type URLOptions struct {
	// Client fetches the archive. The default times out after a minute.
	Client *http.Client
	// Dir is the folder inside the archive that holds the skills. When the
	// archive has one top-level folder, as GitHub's source archives do, Dir
	// is read inside it.
	Dir string
	// MaxDownload caps the archive's size in bytes.
	MaxDownload int64
	// MaxExtracted caps the total size of the files inside it.
	MaxExtracted int64
	// MaxFiles caps the number of entries inside it.
	MaxFiles int
}

func isURL(location string) bool {
	return strings.HasPrefix(location, "https://") || strings.HasPrefix(location, "http://")
}

// LoadURL downloads a .zip or .tar.gz archive and reads the skills in it.
// Nothing is written to disk and nothing in the archive is run.
func LoadURL(ctx context.Context, location string, options URLOptions) (*Pack, error) {
	parsed, err := url.Parse(location)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, errorf("%s is not an http or https address", location)
	}
	if options.Client == nil {
		options.Client = &http.Client{Timeout: time.Minute}
	}
	if options.MaxDownload <= 0 {
		options.MaxDownload = DefaultMaxDownload
	}
	if options.MaxExtracted <= 0 {
		options.MaxExtracted = DefaultMaxExtracted
	}
	if options.MaxFiles <= 0 {
		options.MaxFiles = DefaultMaxFiles
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	response, err := options.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errorf("%s answered %s", location, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, options.MaxDownload+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > options.MaxDownload {
		return nil, errorf("%s is larger than %d bytes", location, options.MaxDownload)
	}
	archive, err := openArchive(data, options)
	if err != nil {
		return nil, errorf("%s: %v", location, err)
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
			skill.folder = archiveName(parsed.Path)
		}
	}
	return pack, nil
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

func openArchive(data []byte, options URLOptions) (fs.FS, error) {
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")), bytes.HasPrefix(data, []byte("PK\x05\x06")):
		reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		if len(reader.File) > options.MaxFiles {
			return nil, errorf("the archive holds more than %d entries", options.MaxFiles)
		}
		var total uint64
		for _, file := range reader.File {
			total += file.UncompressedSize64
		}
		if total > uint64(options.MaxExtracted) {
			return nil, errorf("the archive unpacks to more than %d bytes", options.MaxExtracted)
		}
		return reader, nil
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		return readTarGzip(data, options)
	default:
		return nil, errorf("it is not a .zip or .tar.gz archive")
	}
}

func readTarGzip(data []byte, options URLOptions) (fs.FS, error) {
	unzipped, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer unzipped.Close()
	reader := tar.NewReader(unzipped)
	files := fstest.MapFS{}
	var total int64
	for count := 0; ; count++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if count >= options.MaxFiles {
			return nil, errorf("the archive holds more than %d entries", options.MaxFiles)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name == "" || name == "." {
			continue
		}
		if !fs.ValidPath(name) {
			return nil, errorf("the archive holds an entry outside it: %s", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			files[name] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		case tar.TypeReg:
			total += header.Size
			if header.Size < 0 || total > options.MaxExtracted {
				return nil, errorf("the archive unpacks to more than %d bytes", options.MaxExtracted)
			}
			content, err := io.ReadAll(io.LimitReader(reader, header.Size))
			if err != nil {
				return nil, err
			}
			files[name] = &fstest.MapFile{Data: content, Mode: 0o644}
		}
	}
}
