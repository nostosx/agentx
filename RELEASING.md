# Releasing

agentx and the modules of agentx-contrib have independent semantic versions.
Go 1.27.3 is the development toolchain requirement, not the library version.
Core releases start at v1.0.0. The changelog records release dates and scope.

kitx v1.0.0 is already published. Release the core first, followed by contrib
modules and agentx-examples.
The workflows create tags only after verification. A published module version
cannot be reused; do not create release tags before the checks finish.

## Prepare agentx

1. Use Go 1.27.3 and start from reviewed changes intended for `main`.
   Set `Version` in `agentx.go` to the new version without the `v`; MCP
   servers and telemetry report this value. Move user-visible changes from `Unreleased` into the matching
   `## vX.Y.Z` section in `CHANGELOG.md` and record the release date.
2. Keep the published kitx v1.0.0 dependency, or deliberately select another
   published release. Verify it from a clean consumer module without
   sibling checkouts or replacement directives.
   Run `make check`, `make lint` and `make vulncheck`. The first command
   checks formatting, modernizers, module tidiness, vet, build, the
   standard-library and kitx dependency boundary and shuffled race tests.
3. Review the files intended for publication, including the MIT license,
   README links and security policy. Once the changes are reviewed and
   merged, run the **Release** workflow from `main` with the version.

The release workflow rejects an invalid or existing tag, a mismatched
`agentx.Version`, missing changelog notes, local replacements and unreleased
kitx versions. It runs `make check` on Linux, macOS and Windows, plus lint and
vulnerability scans. Only after all required
jobs pass does a publishing job create the tag and GitHub release at the
checked commit and ask the public Go module proxy for the version.
Verification jobs have read-only permissions; only publishing has
`contents: write`. Releases are serialized.

Proxy warming happens after publication. If it fails, inspect the existing
tag and release before retrying. Verify installation from a clean temporary
module with `GOWORK=off`, the released version and no local replacements.
This check establishes downstream installation independently of local tests.

## Release contrib and update examples

After the core version is available from the public module proxy:

1. Follow [agentx-contrib's release guide](https://github.com/nostosx/agentx-contrib/blob/main/RELEASING.md)
   to replace development dependencies with that released version and verify
   each selected module. A dependency's `replace` is not inherited by its
   consumers.
2. Release each contrib module with its own `<module>/vA.B.C` tag. Store
   releases must pass the applicable conformance suites; `sqlstore` release
   acceptance requires both real PostgreSQL and MySQL backends.
3. Update the independent
   [agentx-examples](https://github.com/nostosx/agentx-examples) project only
   after core, provider and filestore versions are available. Select their
   versions independently and verify without sibling replacements.

A later core release does not require new contrib releases unless a module
needs its new API or fixes.
