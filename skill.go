// Package skillpack loads, validates and serves Agent Skills folders
// (https://agentskills.io/specification) for any model. Skills load from a
// folder on disk, any fs.FS, a discovery index, a SKILL.md URL or an archive URL.
// Loading reads text only; nothing in a skill's scripts/ folder is executed.
package skillpack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LongestBody is the SKILL.md body length, in lines, past which validation
// suggests moving detail into references/.
const LongestBody = 500

var (
	fields      = map[string]bool{"name": true, "description": true, "license": true, "compatibility": true, "metadata": true, "allowed-tools": true}
	namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	linkPattern = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	scheme      = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	frontmatter = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---[ \t]*(?:\r?\n|$)`)
	plainLine   = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):[ \t]+(\S.*)$`)
	// Skipped in every folder of a skill.
	skippedAnywhere = map[string]bool{"__pycache__": true, "node_modules": true}
	// Skipped only as a top-level folder of a skill.
	skippedAtRoot = map[string]bool{"evals": true}
)

// Error reports a skill that cannot be loaded or a request it cannot answer.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func errorf(format string, args ...any) error { return &Error{Message: fmt.Sprintf(format, args...)} }

// Severity separates problems that make a skill invalid from advice.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Problem is one way a skill departs from the specification.
type Problem struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path,omitempty"`
}

// RubricRef names the rubric a skill suggests for grading its work.
type RubricRef struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

// CheckRef is a check a skill declares in checks.yaml.
type CheckRef struct {
	ID          string `json:"id" yaml:"id"`
	Description string `json:"description,omitempty" yaml:"description"`
}

// Skill is one Agent Skills folder: SKILL.md frontmatter and body, plus
// references/, assets/ and scripts/.
type Skill struct {
	// Root says where the skill was loaded from: a folder path, or a
	// location inside an fs.FS or archive. It is for people and messages.
	Root        string
	Frontmatter map[string]any
	Body        string
	// YAMLError is set when the frontmatter only read as plain "key: value"
	// lines. Strict readers reject such a skill, so validation reports it.
	YAMLError string
	// Digest is the sha256 digest a discovery index listed for the skill's
	// artifact, which LoadURL verified. It is empty for skills read any other
	// way.
	Digest string

	indexDescription string
	fsys             fs.FS
	dir              string
	folder           string
	// disk is the skill's folder on disk, when it has one. Symbolic links
	// there are resolved so a file cannot be read from outside the folder.
	disk string
}

// SplitFrontmatter returns the frontmatter mapping and the body after it.
// With lenient, frontmatter that is not valid YAML but reads as plain
// "key: value" lines is accepted and the YAML error returned beside it.
func SplitFrontmatter(text string, lenient bool) (map[string]any, string, string, error) {
	match := frontmatter.FindStringSubmatchIndex(text)
	if match == nil {
		return nil, "", "", errorf("SKILL.md has no YAML frontmatter")
	}
	block := text[match[2]:match[3]]
	var front any
	problem := ""
	if err := yaml.Unmarshal([]byte(block), &front); err != nil {
		plain := map[string]any(nil)
		if lenient {
			plain = plainLines(block)
		}
		if plain == nil {
			return nil, "", "", errorf("Frontmatter is not valid YAML: %v", err)
		}
		front = plain
		problem = strings.Join(strings.Fields(err.Error()), " ")
	}
	mapping, ok := front.(map[string]any)
	if !ok {
		return nil, "", "", errorf("Frontmatter must be a mapping")
	}
	return mapping, text[match[1]:], problem, nil
}

func plainLines(block string) map[string]any {
	found := map[string]any{}
	for _, line := range strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := plainLine.FindStringSubmatch(line)
		if parts == nil {
			return nil
		}
		found[parts[1]] = strings.TrimSpace(parts[2])
	}
	if len(found) == 0 {
		return nil
	}
	return found
}

// LoadSkill reads a skill folder on disk, or a SKILL.md file inside one.
func LoadSkill(location string) (*Skill, error) {
	root, err := filepath.Abs(location)
	if err != nil {
		return nil, err
	}
	if filepath.Base(root) == "SKILL.md" {
		if info, err := os.Stat(root); err == nil && info.Mode().IsRegular() {
			root = filepath.Dir(root)
		}
	}
	skill, err := readSkill(os.DirFS(root), ".", root, filepath.Base(root))
	if err != nil {
		return nil, err
	}
	skill.disk = root
	return skill, nil
}

// LoadSkillFS reads the skill in folder dir of fsys ("." for its root).
func LoadSkillFS(fsys fs.FS, dir string) (*Skill, error) {
	dir = path.Clean(dir)
	if !fs.ValidPath(dir) {
		return nil, errorf("%s is not a folder path inside the file system", dir)
	}
	return readSkill(fsys, dir, dir, path.Base(dir))
}

func readSkill(fsys fs.FS, dir, root, folder string) (*Skill, error) {
	text, err := fs.ReadFile(fsys, path.Join(dir, "SKILL.md"))
	if err != nil {
		return nil, errorf("%s has no SKILL.md", root)
	}
	front, body, problem, err := SplitFrontmatter(string(text), true)
	if err != nil {
		return nil, err
	}
	return &Skill{Root: root, Frontmatter: front, Body: body, YAMLError: problem, fsys: fsys, dir: dir, folder: folder}, nil
}

func (s *Skill) text(key string) string {
	value, _ := s.Frontmatter[key].(string)
	return strings.TrimSpace(value)
}

// Name is the trimmed frontmatter name, or "" when it is missing or not text.
func (s *Skill) Name() string { return s.text("name") }

// Description is the trimmed frontmatter description.
func (s *Skill) Description() string { return s.text("description") }

// Folder is the name of the folder holding SKILL.md, which the name must match.
func (s *Skill) Folder() string { return s.folder }

// FS returns the file system the skill was read from and its folder in it.
func (s *Skill) FS() (fs.FS, string) { return s.fsys, s.dir }

// Metadata is the frontmatter metadata as text; values are formatted when the
// skill declares them as other types, which validation reports.
func (s *Skill) Metadata() map[string]string {
	found := map[string]string{}
	if value, ok := s.Frontmatter["metadata"].(map[string]any); ok {
		for key, item := range value {
			found[key] = fmt.Sprint(item)
		}
	}
	return found
}

// AllowedTools returns the tool names in the space-separated allowed-tools field.
func (s *Skill) AllowedTools() []string {
	value, _ := s.Frontmatter["allowed-tools"].(string)
	return strings.Fields(value)
}

// Rubric is the rubric this skill suggests, from metadata rubric-id and
// rubric-version. The host decides whether to use it.
func (s *Skill) Rubric() *RubricRef {
	metadata := s.Metadata()
	if metadata["rubric-id"] == "" {
		return nil
	}
	return &RubricRef{ID: metadata["rubric-id"], Version: metadata["rubric-version"]}
}

// resolve turns a path relative to the skill folder into one inside fsys,
// refusing any that would leave the folder.
func (s *Skill) resolve(relative string) (string, error) {
	relative = filepath.ToSlash(relative)
	if strings.HasPrefix(relative, "/") || filepath.IsAbs(relative) {
		return "", errorf("%s is outside the skill folder", relative)
	}
	joined := path.Join(s.dir, relative)
	escapes := joined == ".." || strings.HasPrefix(joined, "../")
	if s.dir != "." && joined != s.dir && !strings.HasPrefix(joined, s.dir+"/") {
		escapes = true
	}
	if escapes {
		return "", errorf("%s is outside the skill folder", relative)
	}
	if s.disk != "" {
		if root, err := filepath.EvalSymlinks(s.disk); err == nil {
			if resolved, err := filepath.EvalSymlinks(filepath.Join(s.disk, filepath.FromSlash(relative))); err == nil && !within(root, resolved) {
				return "", errorf("%s is outside the skill folder", relative)
			}
		}
	}
	return joined, nil
}

func within(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *Skill) isFile(name string) bool {
	info, err := fs.Stat(s.fsys, name)
	return err == nil && info.Mode().IsRegular()
}

// Files lists the files under one of the skill's folders, relative to it.
func (s *Skill) Files(folder string) []string {
	found := []string{}
	base, err := s.resolve(folder)
	if err != nil {
		return found
	}
	_ = fs.WalkDir(s.fsys, base, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if name != base && skippedAnywhere[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if s.isFile(name) {
			found = append(found, strings.TrimPrefix(name, base+"/"))
		}
		return nil
	})
	sort.Strings(found)
	return found
}

// References lists the files under references/.
func (s *Skill) References() []string { return s.Files("references") }

// Assets lists the files under assets/.
func (s *Skill) Assets() []string { return s.Files("assets") }

func (s *Skill) read(folder, name string) (string, error) {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", errorf("%s is outside the skill folder", name)
		}
	}
	for _, candidate := range []string{name, name + ".md"} {
		target, err := s.resolve(folder + "/" + candidate)
		if err != nil {
			return "", err
		}
		if s.isFile(target) {
			text, err := fs.ReadFile(s.fsys, target)
			return string(text), err
		}
	}
	known := strings.Join(s.Files(folder), ", ")
	if known == "" {
		known = "none"
	}
	return "", errorf("%s has no %s/%s (has: %s)", s.Name(), folder, name, known)
}

// Reference reads references/<name>, or references/<name>.md.
func (s *Skill) Reference(name string) (string, error) { return s.read("references", name) }

// Asset reads assets/<name>, or assets/<name>.md.
func (s *Skill) Asset(name string) (string, error) { return s.read("assets", name) }

// Render fills {{value}} placeholders in an assets/ template. Every
// placeholder must be given a value.
func (s *Skill) Render(name string, values map[string]any) (string, error) {
	template, err := s.Asset(name)
	if err != nil {
		return "", err
	}
	missing := map[string]bool{}
	for _, match := range placeholder.FindAllStringSubmatch(template, -1) {
		if _, ok := values[match[1]]; !ok {
			missing[match[1]] = true
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", errorf("%s needs values for: %s", name, strings.Join(names, ", "))
	}
	return placeholder.ReplaceAllStringFunc(template, func(found string) string {
		return fmt.Sprint(values[placeholder.FindStringSubmatch(found)[1]])
	}), nil
}

// Links lists the local files SKILL.md links to, relative to the skill root.
func (s *Skill) Links() []string {
	found := []string{}
	seen := map[string]bool{}
	for _, match := range linkPattern.FindAllStringSubmatch(s.Body, -1) {
		target, _, _ := strings.Cut(match[1], "#")
		if target == "" || scheme.MatchString(target) || seen[target] {
			continue
		}
		seen[target] = true
		found = append(found, target)
	}
	return found
}

// Checks lists the checks the skill declares in checks.yaml, in file order.
// Evaluating them is up to the host; this package does not run them.
func (s *Skill) Checks() []CheckRef {
	name, err := s.resolve("checks.yaml")
	if err != nil {
		return nil
	}
	text, err := fs.ReadFile(s.fsys, name)
	if err != nil {
		return nil
	}
	var file struct {
		Checks []CheckRef `yaml:"checks"`
	}
	if yaml.Unmarshal(text, &file) != nil {
		return nil
	}
	return file.Checks
}

// Validate reports problems against the Agent Skills specification. Errors
// make the skill invalid; warnings are advice.
func (s *Skill) Validate() []Problem {
	problems := []Problem{}
	add := func(code, message string, severity Severity, path string) {
		problems = append(problems, Problem{Code: code, Message: message, Severity: severity, Path: path})
	}
	if s.YAMLError != "" {
		add("frontmatter-yaml", "Frontmatter is not valid YAML, so strict readers will reject the skill; quote values that contain ': ' ("+s.YAMLError+")", SeverityError, "")
	}
	extra := []string{}
	for key := range s.Frontmatter {
		if !fields[key] {
			extra = append(extra, key)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		add("unknown-field", "Frontmatter fields not in the spec: "+strings.Join(extra, ", "), SeverityError, "")
	}

	if name, ok := s.Frontmatter["name"].(string); !ok || strings.TrimSpace(name) == "" {
		add("name-missing", "Frontmatter needs a name", SeverityError, "")
	} else {
		name = strings.TrimSpace(name)
		if length := len([]rune(name)); length > 64 {
			add("name-length", fmt.Sprintf("Name is %d characters; the limit is 64", length), SeverityError, "")
		}
		if !namePattern.MatchString(name) {
			add("name-format", fmt.Sprintf("Name %q must be lowercase letters, digits and single hyphens, not starting or ending with a hyphen", name), SeverityError, "")
		}
		if name != s.folder {
			add("name-folder", fmt.Sprintf("Name %q must match its folder %q", name, s.folder), SeverityError, "")
		}
	}

	if description, ok := s.Frontmatter["description"].(string); !ok || strings.TrimSpace(description) == "" {
		add("description-missing", "Frontmatter needs a description", SeverityError, "")
	} else {
		if length := len([]rune(strings.TrimSpace(description))); length > 1024 {
			add("description-length", fmt.Sprintf("Description is %d characters; the limit is 1024", length), SeverityError, "")
		}
		if strings.ContainsAny(description, "<>") {
			add("description-brackets", "Description cannot contain < or >", SeverityError, "")
		}
	}

	if value, present := s.Frontmatter["compatibility"]; present && value != nil {
		if text, ok := value.(string); !ok {
			add("compatibility-type", "Compatibility must be text", SeverityError, "")
		} else if length := len([]rune(text)); length > 500 {
			add("compatibility-length", fmt.Sprintf("Compatibility is %d characters; the limit is 500", length), SeverityError, "")
		}
	}

	if value, present := s.Frontmatter["metadata"]; present && value != nil {
		mapping, ok := value.(map[string]any)
		for _, item := range mapping {
			if _, text := item.(string); !text {
				ok = false
			}
		}
		if !ok {
			add("metadata-type", "Metadata must map text keys to text values", SeverityError, "")
		}
	}

	if value, present := s.Frontmatter["allowed-tools"]; present && value != nil {
		if _, ok := value.(string); !ok {
			add("allowed-tools-type", "allowed-tools must be a space-separated string", SeverityError, "")
		}
	}

	if nested := s.nestedSkills(); len(nested) > 0 {
		add("nested-skill", "A skill holds one SKILL.md; also found: "+strings.Join(nested, ", "), SeverityError, "")
	}

	for _, target := range s.Links() {
		name, err := s.resolve(target)
		if err != nil {
			add("link-outside", target+" points outside the skill folder", SeverityError, target)
			continue
		}
		if _, err := fs.Stat(s.fsys, name); errors.Is(err, fs.ErrNotExist) {
			add("link-missing", "SKILL.md links to "+target+", which does not exist", SeverityError, target)
		}
	}

	if s.indexDescription != "" && strings.TrimSpace(s.indexDescription) != s.Description() {
		add("index-description", "The discovery index describes this skill differently from its SKILL.md", SeverityWarning, "")
	}

	if lines := strings.Count(s.Body, "\n") + 1; lines > LongestBody {
		add("body-length", fmt.Sprintf("SKILL.md body is %d lines; keep it under %d and move detail into references/", lines, LongestBody), SeverityWarning, "")
	}
	return problems
}

func (s *Skill) nestedSkills() []string {
	found := []string{}
	_ = fs.WalkDir(s.fsys, s.dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		relative := name
		if s.dir != "." {
			relative = strings.TrimPrefix(strings.TrimPrefix(name, s.dir), "/")
		}
		if entry.IsDir() {
			if name != s.dir && (skippedAnywhere[entry.Name()] || (!strings.Contains(relative, "/") && skippedAtRoot[entry.Name()])) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() == "SKILL.md" && relative != "SKILL.md" {
			found = append(found, relative)
		}
		return nil
	})
	sort.Strings(found)
	return found
}

// Valid reports whether validation found no errors.
func (s *Skill) Valid() bool {
	for _, problem := range s.Validate() {
		if problem.Severity == SeverityError {
			return false
		}
	}
	return true
}
