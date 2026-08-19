# Changelog

Every user-visible change lands here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/spec/v2.0.0.html).

Section conventions:

- **Added** — new features, endpoints, subcommands, config knobs.
- **Changed** — behavior changes that a user or integrator would
  notice (URL renames, default flips, response-shape edits).
- **Removed** — features / endpoints / knobs gone.
- **Fixed** — bug fixes.
- **Security** — vulnerabilities patched, defense-in-depth
  tightenings. Always call these out separately.

Migration notes get their own **⚠ Migration** callouts inside a
section when they need operator action (env-var renames, schema
changes that require a restart, etc.).

## [Unreleased]

Everything since the last tag lands here and rolls into the next
version header when a tag is cut.

### Changed

- Built-in automations now use plain-language tuning views and confidence sliders while keeping their fixed workflow structure read-only.

### Fixed

- Taxonomy CLI imports now honor flags after the input filename, apply merge-mode imports with explicit collision remaps, and export the same portable seeds as the admin API.
- Beta and release-candidate images now receive documented moving channel tags instead of leaving prerelease quick-start commands pointed at an unpublished `latest` image.

---

## Release history

No tags cut yet. `v0.1` is the first line in the sand.
