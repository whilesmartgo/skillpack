# skillpack

Load, check and serve [Agent Skills](https://agentskills.io/specification) folders from Go, with
any model.

A skill is a folder with a `SKILL.md` (YAML frontmatter with `name` and `description`, then
markdown guidance) and optional `references/`, `assets/` and `scripts/`. skillpack ships no skills
of its own: it reads them from wherever you keep them, validates them against the spec and hands
them to a model a piece at a time. Loading a skill never runs anything in it.

```sh
go get github.com/whilesmartgo/skillpack
```

## Where skills come from

```go
skillpack.Load("skills", "/opt/team-skills")         // folders on disk; symbolic links are followed
skillpack.Load("https://example.com/skills.tar.gz")  // a .zip or .tar.gz archive at a URL
skillpack.LoadFS(embedded, "skills")                 // any fs.FS: embed.FS, an opened zip, os.DirFS
skillpack.LoadURL(ctx, "https://github.com/acme/skills/archive/refs/heads/main.zip",
	skillpack.URLOptions{Dir: "skills", Client: client})
```

A location holding `SKILL.md` is one skill; otherwise each folder inside it holding `SKILL.md` is.
Archives are read in memory, never written to disk. When an archive has a single top-level
folder, as GitHub's source archives do, `Dir` is read inside it. Downloads are capped at 32 MiB,
128 MiB unpacked and 5000 entries unless `URLOptions` says otherwise. A skill on disk cannot read
a file outside its folder, even through a symbolic link.

## Loading and validating

```go
pack, err := skillpack.Load("skills")
skill, err := pack.Get("page-layout")

skill.Validate() // []Problem{{Code: "name-folder", Message: "Name ... must match ...", Severity: "error"}}
skill.Valid()    // false when any problem is an error
```

Validation follows the spec: name format and length, the name matching its folder, description
and compatibility lengths, metadata as text to text, no unknown frontmatter fields, one `SKILL.md`
per folder, and every local link in `SKILL.md` pointing at a file that exists. Frontmatter that is
not valid YAML (often an unquoted `: ` in a description) still loads, as plain `key: value`
lines, and is reported as an error because strict readers reject it.

## Giving skills to a model

```go
pack.Index()                      // []Entry{{Name: ..., Description: ...}}
skill.Body                        // the markdown after the frontmatter
skill.Reference("grids")          // references/grids.md
skill.Render("section", map[string]any{"title": "Home"}) // assets/section.md with {{title}} filled
```

`pack.Tools()` returns `list_skills`, `read_skill` and `read_reference`, each with a name,
description and JSON Schema, for any agent loop. `Call` refuses arguments the schema does not
declare.

```go
for _, tool := range pack.Tools() {
	register(tool.Name, tool.Description, tool.Parameters)
}
result, err := tool.Call(ctx, call.Arguments) // call.Arguments is the model's JSON
```

`pack.Install(".claude/skills")` copies the folders where Claude Code looks for them.

`skillpack.Guidance(skills, task, options)` builds a prompt that offers skills as craft to draw on
rather than steps to follow, lists each skill's checks, and asks the model to record a departure
(check id, items, reason) when it goes against one on purpose. `NamesOnly` lists skills by name
and description only, for a model that reads them through the tools. `AlreadyUsed` names choices
recent work already made, so the next piece does not repeat them.

## Checks

A skill may declare checks in `checks.yaml`. `skill.Checks()` lists their ids and descriptions,
which `Guidance` passes to the model. Running checks, workflows and evals is not in this version
yet; the Python package documents their format.

## License

MIT
