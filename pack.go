package skillpack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Pack holds skills by name, in the order they were loaded.
type Pack struct {
	order    []*Skill
	byName   map[string]*Skill
	warnings []string
}

// NewPack collects skills, refusing two with the same name.
func NewPack(skills []*Skill) (*Pack, error) {
	pack := &Pack{byName: map[string]*Skill{}}
	for _, skill := range skills {
		if existing, ok := pack.byName[skill.Name()]; ok {
			return nil, errorf("Two skills are named %s: %s and %s", skill.Name(), existing.Root, skill.Root)
		}
		pack.byName[skill.Name()] = skill
		pack.order = append(pack.order, skill)
	}
	return pack, nil
}

// Load reads skills from each location: a folder on disk, or an http(s) URL
// that LoadURL reads. A folder holding SKILL.md is one skill; otherwise each
// child folder holding SKILL.md is. Use LoadURL to pass a context, an HTTP
// client, an allowlist of hosts or limits for downloads.
func Load(locations ...string) (*Pack, error) {
	skills := []*Skill{}
	warnings := []string{}
	for _, location := range locations {
		if isURL(location) {
			pack, err := LoadURL(context.Background(), location, URLOptions{})
			if err != nil {
				return nil, err
			}
			skills = append(skills, pack.order...)
			warnings = append(warnings, pack.warnings...)
			continue
		}
		found, err := loadFolder(location)
		if err != nil {
			return nil, err
		}
		skills = append(skills, found...)
	}
	pack, err := NewPack(skills)
	if err != nil {
		return nil, err
	}
	pack.warnings = warnings
	return pack, nil
}

func loadFolder(location string) ([]*Skill, error) {
	if info, err := os.Stat(filepath.Join(location, "SKILL.md")); err == nil && info.Mode().IsRegular() {
		skill, err := LoadSkill(location)
		if err != nil {
			return nil, err
		}
		return []*Skill{skill}, nil
	}
	entries, err := os.ReadDir(location)
	if err != nil {
		return nil, errorf("%s is not a folder", location)
	}
	skills := []*Skill{}
	for _, entry := range entries {
		child := filepath.Join(location, entry.Name())
		if info, err := os.Stat(child); err != nil || !info.IsDir() {
			continue
		}
		if info, err := os.Stat(filepath.Join(child, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
			continue
		}
		skill, err := LoadSkill(child)
		if err != nil {
			return nil, err
		}
		skills = append(skills, skill)
	}
	return skills, nil
}

// LoadFS reads skills from folders of fsys, such as an embed.FS, an opened
// zip file or os.DirFS. With no folders it reads fsys's root. A folder holding
// SKILL.md is one skill; otherwise each child folder holding SKILL.md is.
func LoadFS(fsys fs.FS, dirs ...string) (*Pack, error) {
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	skills := []*Skill{}
	for _, dir := range dirs {
		dir = path.Clean(dir)
		if !fs.ValidPath(dir) {
			return nil, errorf("%s is not a folder path inside the file system", dir)
		}
		if info, err := fs.Stat(fsys, path.Join(dir, "SKILL.md")); err == nil && info.Mode().IsRegular() {
			skill, err := LoadSkillFS(fsys, dir)
			if err != nil {
				return nil, err
			}
			skills = append(skills, skill)
			continue
		}
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return nil, errorf("%s is not a folder", dir)
		}
		for _, entry := range entries {
			child := path.Join(dir, entry.Name())
			if info, err := fs.Stat(fsys, child); err != nil || !info.IsDir() {
				continue
			}
			if info, err := fs.Stat(fsys, path.Join(child, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
				continue
			}
			skill, err := LoadSkillFS(fsys, child)
			if err != nil {
				return nil, err
			}
			skills = append(skills, skill)
		}
	}
	return NewPack(skills)
}

// Warnings lists what loading skipped without failing, such as discovery
// index entries of a type this version does not read.
func (p *Pack) Warnings() []string { return append([]string(nil), p.warnings...) }

// Skills returns the skills in load order.
func (p *Pack) Skills() []*Skill { return append([]*Skill(nil), p.order...) }

// Len is the number of skills.
func (p *Pack) Len() int { return len(p.order) }

// Has reports whether a skill has the name.
func (p *Pack) Has(name string) bool { _, ok := p.byName[name]; return ok }

// Get returns the named skill, or an error listing the names it has.
func (p *Pack) Get(name string) (*Skill, error) {
	if skill, ok := p.byName[name]; ok {
		return skill, nil
	}
	names := make([]string, 0, len(p.order))
	for _, skill := range p.order {
		names = append(names, skill.Name())
	}
	known := strings.Join(names, ", ")
	if known == "" {
		known = "none"
	}
	return nil, errorf("No skill named %s (have: %s)", name, known)
}

// Entry is one line of the short list a model chooses skills from.
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Index lists the name and description of every skill.
func (p *Pack) Index() []Entry {
	entries := make([]Entry, 0, len(p.order))
	for _, skill := range p.order {
		entries = append(entries, Entry{Name: skill.Name(), Description: skill.Description()})
	}
	return entries
}

// IndexText is the index as "- name: description" lines.
func (p *Pack) IndexText() string {
	lines := make([]string, 0, len(p.order))
	for _, entry := range p.Index() {
		lines = append(lines, "- "+entry.Name+": "+entry.Description)
	}
	return strings.Join(lines, "\n")
}

// Problems returns validation problems by skill name, leaving out skills with none.
func (p *Pack) Problems() map[string][]Problem {
	found := map[string][]Problem{}
	for _, skill := range p.order {
		if problems := skill.Validate(); len(problems) > 0 {
			found[skill.Name()] = problems
		}
	}
	return found
}

// Install copies every skill folder into target, such as a .claude/skills
// folder, replacing a folder of the same name. Symbolic links are copied as
// links.
func (p *Pack) Install(target string) ([]string, error) {
	unnamed := []string{}
	for _, skill := range p.order {
		if !namePattern.MatchString(skill.Name()) {
			unnamed = append(unnamed, skill.Root)
		}
	}
	if len(unnamed) > 0 {
		return nil, errorf("Skills without a valid name cannot be installed: %s", strings.Join(unnamed, ", "))
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}
	written := []string{}
	for _, skill := range p.order {
		destination := filepath.Join(target, skill.Name())
		if err := os.RemoveAll(destination); err != nil {
			return written, err
		}
		var copied error
		if skill.disk != "" {
			copied = copyTree(skill.disk, destination)
		} else {
			copied = copyFS(skill.fsys, skill.dir, destination)
		}
		if copied != nil {
			return written, copied
		}
		written = append(written, destination)
	}
	return written, nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != source && skippedAnywhere[entry.Name()] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

func copyFS(fsys fs.FS, dir, destination string) error {
	return fs.WalkDir(fsys, dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name != dir && skippedAnywhere[entry.Name()] {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relative := name
		if dir != "." {
			relative = strings.TrimPrefix(strings.TrimPrefix(name, dir), "/")
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := fs.Stat(fsys, name)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		in, err := fsys.Open(name)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func copyFile(source, target string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Tool is a function an agent loop registers: a name, a description, a JSON
// Schema for its arguments and the function that answers a call.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	call        func(arguments map[string]string) (string, error)
	required    []string
	allowed     map[string]bool
}

// Schema is the tool as {"name", "description", "parameters"}.
func (t Tool) Schema() map[string]any {
	return map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}
}

// Call answers a model's call with its JSON arguments. Arguments the schema
// does not declare, missing required ones and non-text values are refused.
func (t Tool) Call(ctx context.Context, arguments json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	values := map[string]string{}
	if trimmed := bytes.TrimSpace(arguments); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		raw := map[string]any{}
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return "", errorf("%s arguments must be a JSON object", t.Name)
		}
		for key, value := range raw {
			if !t.allowed[key] {
				return "", errorf("%s does not take %s", t.Name, key)
			}
			text, ok := value.(string)
			if !ok {
				return "", errorf("%s needs %s as text", t.Name, key)
			}
			values[key] = text
		}
	}
	for _, key := range t.required {
		if _, ok := values[key]; !ok {
			return "", errorf("%s needs %s", t.Name, key)
		}
	}
	return t.call(values)
}

// Tools returns list_skills, read_skill and read_reference for a model that
// reads skills on demand.
func (p *Pack) Tools() []Tool {
	names := make([]string, 0, len(p.order))
	for _, skill := range p.order {
		names = append(names, skill.Name())
	}
	sort.Strings(names)
	enum, _ := json.Marshal(names)
	schema := func(properties string, required ...string) json.RawMessage {
		requiredJSON, _ := json.Marshal(required)
		if len(required) == 0 {
			return json.RawMessage(`{"type":"object","properties":{` + properties + `},"additionalProperties":false}`)
		}
		return json.RawMessage(`{"type":"object","properties":{` + properties + `},"required":` + string(requiredJSON) + `,"additionalProperties":false}`)
	}
	set := func(keys ...string) map[string]bool {
		found := map[string]bool{}
		for _, key := range keys {
			found[key] = true
		}
		return found
	}
	return []Tool{
		{
			Name:        "list_skills",
			Description: "List the skills you can draw on, each with when it applies.",
			Parameters:  schema(""),
			allowed:     set(),
			call:        func(map[string]string) (string, error) { return p.IndexText(), nil },
		},
		{
			Name:        "read_skill",
			Description: "Read a skill's guidance and the references it offers.",
			Parameters:  schema(`"name":{"type":"string","enum":`+string(enum)+`}`, "name"),
			required:    []string{"name"},
			allowed:     set("name"),
			call: func(arguments map[string]string) (string, error) {
				skill, err := p.Get(arguments["name"])
				if err != nil {
					return "", err
				}
				parts := []string{strings.TrimSpace(skill.Body)}
				if references := skill.References(); len(references) > 0 {
					parts = append(parts, "References: "+strings.Join(references, ", "))
				}
				return strings.Join(parts, "\n\n"), nil
			},
		},
		{
			Name:        "read_reference",
			Description: "Read one reference file from a skill, by the name read_skill lists.",
			Parameters:  schema(`"skill":{"type":"string","enum":`+string(enum)+`},"name":{"type":"string"}`, "skill", "name"),
			required:    []string{"skill", "name"},
			allowed:     set("skill", "name"),
			call: func(arguments map[string]string) (string, error) {
				skill, err := p.Get(arguments["skill"])
				if err != nil {
					return "", err
				}
				return skill.Reference(arguments["name"])
			},
		},
	}
}
