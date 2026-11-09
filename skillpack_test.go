package skillpack

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func codes(problems []Problem) []string {
	found := []string{}
	for _, problem := range problems {
		found = append(found, problem.Code)
	}
	sort.Strings(found)
	return found
}

func mustLoad(t *testing.T, paths ...string) *Pack {
	t.Helper()
	pack, err := Load(paths...)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func TestLoadReadsEachChildSkillInNameOrder(t *testing.T) {
	pack := mustLoad(t, "testdata/skills")
	want := []Entry{
		{Name: "data-modelling", Description: "Model entities and their relations before writing tables."},
		{Name: "page-layout", Description: "Lay out a web page so its most important action is found first, at any screen width."},
	}
	if got := pack.Index(); !reflect.DeepEqual(got, want) {
		t.Fatalf("index = %#v", got)
	}
	if got := pack.IndexText(); !strings.HasPrefix(got, "- data-modelling: Model entities") || strings.Count(got, "\n") != 1 {
		t.Fatalf("index text = %q", got)
	}
	if problems := pack.Problems(); len(problems) != 0 {
		t.Fatalf("valid skills reported problems: %#v", problems)
	}
}

func TestLoadAcceptsASkillFolderOrItsSkillFile(t *testing.T) {
	for _, path := range []string{"testdata/skills/page-layout", "testdata/skills/page-layout/SKILL.md"} {
		skill, err := LoadSkill(path)
		if err != nil {
			t.Fatal(err)
		}
		if skill.Name() != "page-layout" || !skill.Valid() {
			t.Fatalf("%s: name %q, problems %#v", path, skill.Name(), skill.Validate())
		}
	}
}

func TestARelativeSkillIsCheckedAgainstItsRealFolder(t *testing.T) {
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir("testdata/skills/page-layout"); err != nil {
		t.Fatal(err)
	}
	skill, err := LoadSkill(".")
	if err != nil {
		t.Fatal(err)
	}
	if !skill.Valid() {
		t.Fatalf("problems: %#v", skill.Validate())
	}
}

func TestMetadataRubricAndChecks(t *testing.T) {
	skill, _ := LoadSkill("testdata/skills/page-layout")
	if got := skill.Rubric(); got == nil || *got != (RubricRef{ID: "layout-review", Version: "2"}) {
		t.Fatalf("rubric = %#v", got)
	}
	want := []CheckRef{{ID: "primary-action", Description: "The page's main action is visible without scrolling"}, {ID: "tap-targets"}}
	if got := skill.Checks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %#v", got)
	}
	if got := skill.Links(); !reflect.DeepEqual(got, []string{"references/grids.md"}) {
		t.Fatalf("links = %#v", got)
	}
}

func TestReferencesAndTemplates(t *testing.T) {
	skill, _ := LoadSkill("testdata/skills/page-layout")
	if got := skill.References(); !reflect.DeepEqual(got, []string{"grids.md"}) {
		t.Fatalf("references = %#v", got)
	}
	text, err := skill.Reference("grids")
	if err != nil || !strings.HasPrefix(text, "# Grids") {
		t.Fatalf("reference = %q, %v", text, err)
	}
	if _, err := skill.Reference("../SKILL.md"); err == nil || !strings.Contains(err.Error(), "outside the skill folder") {
		t.Fatalf("escaping reference read: %v", err)
	}
	if _, err := skill.Reference("missing"); err == nil || !strings.Contains(err.Error(), "(has: grids.md)") {
		t.Fatalf("missing reference: %v", err)
	}
	if _, err := skill.Render("section", map[string]any{"title": "Home"}); err == nil || err.Error() != "section needs values for: count" {
		t.Fatalf("render without every value: %v", err)
	}
	got, err := skill.Render("section", map[string]any{"title": "Home", "count": 3})
	if err != nil || got != "Section for Home with 3 cards.\n" {
		t.Fatalf("render = %q, %v", got, err)
	}
}

func TestValidationReportsEachSpecProblem(t *testing.T) {
	skill, err := LoadSkill("testdata/broken/bad-skill")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"allowed-tools-type", "compatibility-type", "description-brackets", "link-missing", "link-outside", "metadata-type", "name-folder", "name-format", "unknown-field"}
	if got := codes(skill.Validate()); !reflect.DeepEqual(got, want) {
		t.Fatalf("codes = %#v", got)
	}
	if skill.Valid() {
		t.Fatal("a skill with errors is valid")
	}
}

func TestNestedSkillsIgnoreEvalsAndDependencies(t *testing.T) {
	skill, _ := LoadSkill("testdata/broken/nested")
	problems := skill.Validate()
	if len(problems) != 1 || problems[0].Code != "nested-skill" || problems[0].Message != "A skill holds one SKILL.md; also found: inner/SKILL.md" {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestPlainFrontmatterLoadsButIsAnError(t *testing.T) {
	skill, err := LoadSkill("testdata/broken/plain-yaml")
	if err != nil {
		t.Fatal(err)
	}
	if skill.Description() != "Use when: the colon breaks YAML" {
		t.Fatalf("description = %q", skill.Description())
	}
	if got := codes(skill.Validate()); !reflect.DeepEqual(got, []string{"frontmatter-yaml"}) {
		t.Fatalf("codes = %#v", got)
	}
	if _, _, _, err := SplitFrontmatter("---\nname: a: b\n---\n", false); err == nil {
		t.Fatal("strict split accepted invalid YAML")
	}
	if _, _, _, err := SplitFrontmatter("no frontmatter", true); err == nil {
		t.Fatal("text without frontmatter loaded")
	}
}

func TestLongBodiesAreAWarning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "long-skill")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: long-skill\ndescription: Long.\n---\n" + strings.Repeat("line\n", LongestBody+1)
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	skill, _ := LoadSkill(root)
	problems := skill.Validate()
	if len(problems) != 1 || problems[0].Code != "body-length" || problems[0].Severity != SeverityWarning || !skill.Valid() {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestDuplicateNamesAreRefused(t *testing.T) {
	if _, err := Load("testdata/skills", "testdata/skills/page-layout"); err == nil || !strings.Contains(err.Error(), "Two skills are named page-layout") {
		t.Fatalf("duplicate: %v", err)
	}
	pack := mustLoad(t, "testdata/skills")
	if _, err := pack.Get("nope"); err == nil || err.Error() != "No skill named nope (have: data-modelling, page-layout)" {
		t.Fatalf("get: %v", err)
	}
}

func TestToolsServeSkillsToAModel(t *testing.T) {
	pack := mustLoad(t, "testdata/skills")
	tools := map[string]Tool{}
	for _, tool := range pack.Tools() {
		tools[tool.Name] = tool
		var schema map[string]any
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil || schema["type"] != "object" {
			t.Fatalf("%s schema %s: %v", tool.Name, tool.Parameters, err)
		}
	}
	ctx := context.Background()
	if got, _ := tools["list_skills"].Call(ctx, nil); got != pack.IndexText() {
		t.Fatalf("list_skills = %q", got)
	}
	got, err := tools["read_skill"].Call(ctx, json.RawMessage(`{"name":"page-layout"}`))
	if err != nil || !strings.HasPrefix(got, "# Page layout") || !strings.HasSuffix(got, "\n\nReferences: grids.md") {
		t.Fatalf("read_skill = %q, %v", got, err)
	}
	got, err = tools["read_reference"].Call(ctx, json.RawMessage(`{"skill":"page-layout","name":"grids"}`))
	if err != nil || !strings.HasPrefix(got, "# Grids") {
		t.Fatalf("read_reference = %q, %v", got, err)
	}
	for _, bad := range []string{`{}`, `{"name":"page-layout","extra":"x"}`, `{"name":3}`, `[]`} {
		if _, err := tools["read_skill"].Call(ctx, json.RawMessage(bad)); err == nil {
			t.Fatalf("read_skill accepted %s", bad)
		}
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(tools["read_skill"].Parameters, &schema)
	if !reflect.DeepEqual(schema.Properties["name"].Enum, []string{"data-modelling", "page-layout"}) {
		t.Fatalf("enum = %#v", schema.Properties["name"].Enum)
	}
}

func TestGuidanceOffersSkillsAsCraft(t *testing.T) {
	pack := mustLoad(t, "testdata/skills")
	skill, _ := pack.Get("page-layout")
	got := Guidance([]*Skill{skill}, "  Build a pricing page  ", GuidanceOptions{AlreadyUsed: []string{"hero on the left", " "}})
	for _, part := range []string{
		"## Task\n\nBuild a pricing page\n\n## Skill: page-layout\n\nLay out a web page",
		"# Page layout",
		"Checks for this skill:\n- primary-action: The page's main action is visible without scrolling\n- tap-targets\n\n",
		"## Already used\n\nRecent work already made these choices. Pick something else unless the task asks for one of them:\n- hero on the left\n\n",
		`{"check": "<check id>", "items": ["<what it affects>"], "why": "<reason>"}.`,
	} {
		if !strings.Contains(got, part) {
			t.Fatalf("guidance lacks %q:\n%s", part, got)
		}
	}
	if short := Guidance([]*Skill{skill}, "x", GuidanceOptions{NamesOnly: true}); strings.Contains(short, "# Page layout") {
		t.Fatal("names-only guidance included the body")
	}
}

func TestInstallCopiesSkillsWithoutDependencies(t *testing.T) {
	pack := mustLoad(t, "testdata/skills")
	target := t.TempDir()
	stale := filepath.Join(target, "page-layout", "stale.md")
	_ = os.MkdirAll(filepath.Dir(stale), 0o755)
	_ = os.WriteFile(stale, []byte("old"), 0o644)
	written, err := pack.Install(target)
	if err != nil || len(written) != 2 {
		t.Fatalf("install = %#v, %v", written, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("install kept a file from the previous copy")
	}
	installed, err := LoadSkill(filepath.Join(target, "page-layout"))
	if err != nil || !installed.Valid() || len(installed.References()) != 1 {
		t.Fatalf("installed skill: %v", err)
	}
	broken := mustLoad(t, "testdata/broken/bad-skill")
	if _, err := broken.Install(t.TempDir()); err == nil {
		t.Fatal("installed a skill without a valid name")
	}
}
