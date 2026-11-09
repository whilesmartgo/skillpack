# skillpack

Load, check and serve [Agent Skills](https://agentskills.io/specification) from Go, with any model.
Skills can come from a folder, any `fs.FS`, a `SKILL.md` link, a `.zip` or `.tar.gz` archive, or
an Agent Skills Discovery index.

A skill is a folder with a `SKILL.md` (YAML frontmatter with `name` and `description`, then
markdown guidance) and optional `references/`, `assets/` and `scripts/`. skillpack ships no skills
of its own. It loads them from where you keep them, checks them against the spec, and gives a
model their contents a piece at a time. Loading a skill never runs anything in it.

```sh
go get github.com/whilesmartgo/skillpack
```

## Where skills come from

```go
skillpack.Load("skills", "/opt/team-skills")      // folders on disk
skillpack.Load("https://example.com")             // a site's published skills index
skillpack.LoadFS(embedded, "skills")              // any fs.FS: embed.FS, os.DirFS, ...
skillpack.LoadURL(ctx, "https://github.com/acme/skills/archive/refs/heads/main.zip",
	skillpack.URLOptions{Dir: "skills", AllowedHosts: []string{"github.com", "codeload.github.com"}})
```

On disk, a folder holding `SKILL.md` is one skill; otherwise each folder inside it holding
`SKILL.md` is. Symbolic links are followed, but reading a skill's references, assets or
`checks.yaml` through a link that points outside its folder fails.

A URL is read by what it returns:

| The URL returns | skillpack reads it as |
|---|---|
| JSON | An Agent Skills Discovery index (see below) |
| Markdown | One `SKILL.md`. The skill's folder name comes from the URL: `.../code-review/SKILL.md` and `.../code-review.md` both give `code-review` |
| A `.zip` or `.tar.gz` | The skills inside it. When the archive has a single top-level folder, as GitHub's source archives do, `Dir` is read inside that folder |
| Nothing skillpack recognizes | An error naming the address and its `Content-Type` |

A bare site address such as `https://example.com` is read from
`https://example.com/.well-known/agent-skills/index.json`.

skillpack decides what a response holds from its `Content-Type`, then from the URL's extension,
then from its first bytes. `text/plain` and `application/octet-stream` say nothing, because hosts
such as raw.githubusercontent.com send them for Markdown and JSON alike, so those fall through to
the extension and the bytes.

Everything is read in memory and nothing is written to disk. By default each download is limited
to 32 MiB, each archive to 128 MiB unpacked and 5000 entries, and each index to 500 skills.
`URLOptions` changes these limits, the HTTP client and the hosts allowed.

## Agent Skills Discovery

skillpack reads indexes in the
[Agent Skills Discovery](https://github.com/cloudflare/agent-skills-discovery-rfc) v0.2.0 format.
The RFC is a draft. This is how skillpack handles each of its client requirements:

| The RFC says | skillpack |
|---|---|
| Fetch `/.well-known/agent-skills/index.json` | Does this for a bare site address. An index at any other URL is read too |
| Check `$schema`; do not process an unknown one | Accepts only `https://schemas.agentskills.io/discovery/0.2.0/schema.json` and refuses any other value |
| Treat an index without `$schema` as v0.1.0 | Refuses it, because a v0.1.0 index has no digests and v0.2.0 forbids using content that is not verified |
| Ignore unrecognized fields | Ignores them |
| Resolve `url` against the index URL (RFC 3986) | Resolves relative, path-absolute and absolute URLs, then accepts only http and https |
| Verify each artifact's sha256 `digest`; never use unverified content | Checks each listed skill's download against its digest, and one mismatch fails the whole load. `Skill.Digest` keeps the verified digest |
| Skip an entry with an unknown `type`, and warn | Skips it and adds a line to `Pack.Warnings()` |
| `name` follows the Agent Skills naming rules; `description` is at most 1024 characters | Fails the load when an entry breaks either rule |
| `description` should match the `SKILL.md` frontmatter | `Validate` reports `index-description` as a warning when the two differ |
| The entry's `name` identifies the skill | Is used as the skill's folder name, so `Validate` reports `name-folder` when the `SKILL.md` name differs |
| Support `.tar.gz` and `.zip` archives, using `Content-Type` and then the extension | Supports both, and falls back to the first bytes when neither the type nor the extension decides |
| `SKILL.md` sits at the archive root, with no wrapper folder | Requires this for archives listed in an index. Only an archive URL given directly is read inside a single top-level folder |
| Reject `..` and absolute paths | Refuses the archive |
| Reject symbolic and hard links that resolve outside the skill | Refuses the archive. Links that stay inside are left out, not followed |
| Limit the unpacked size | Limits each archive's total unpacked size and number of entries, and refuses an entry that is larger than its header says |
| Follow redirects | Follows them, up to 10 with the default client |
| Respect cache headers; use digests to detect changes | Keeps no cache, so every load fetches again. A host that caches can compare `Skill.Digest` between loads |
| Load `name` and `description` first and the rest on demand | Loads every listed skill when the index is read. `Pack.Index()` and the `list_skills` tool give a model only names and descriptions |
| Allow only trusted origins | `URLOptions.AllowedHosts` limits the index, each skill and every redirect to the hosts named. With no list, any host is allowed |
| Do not run files under `scripts/` | Runs nothing |

Publishing an index is not part of skillpack yet.

## Loading and validating

```go
pack, err := skillpack.Load("skills")
skill, err := pack.Get("page-layout")

skill.Validate() // []Problem{{Code: "name-folder", Message: "Name ... must match ...", Severity: "error"}}
skill.Valid()    // false when any problem is an error
```

Validation checks the name's format and length and that it matches its folder. It checks the
description and compatibility lengths, that metadata maps text to text, that the frontmatter has
no fields outside the spec, and that the folder holds no second `SKILL.md`. Each local path in a
Markdown link in `SKILL.md` must exist inside the skill folder.

Frontmatter that is not valid YAML, often because of an unquoted `: ` in a description, still
loads when every line reads as plain `key: value`. Validation reports it as an error, because
strict readers reject it.

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
(check id, items, reason) when it goes against one on purpose. `NamesOnly` leaves out skill
bodies and keeps names, descriptions and checks, for a model that reads the bodies through the
tools. `AlreadyUsed` lists choices recent work already made and asks the model to pick others
unless the task needs one of them.

## Checks

A skill may declare checks in `checks.yaml`. `skill.Checks()` lists their ids and descriptions,
which `Guidance` passes to the model. Running checks, workflows and evals is not in this version
yet; the Python package documents their format.

## License

MIT
