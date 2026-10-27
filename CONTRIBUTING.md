# Contributing

Requires Go 1.25+. Build, test, lint and code generation commands are in the
`Makefile`.

To connect to a different server, set `SAND_BACKEND_URL` (API address) and
`SAND_CURSOR_WEBSITE_URL` (sign-in page address).

Shell tests run against bash and, when found, zsh: set `GROK_TEST_ZSH` to a zsh
binary or put zsh on `PATH`.

`testdata/compat/0.75.8/` holds reference data from the desktop client;
`scripts/compat-reference.mjs` regenerates it. `scripts/extract-proto.py`
reconstructs `.proto` schemas from the client's bundle without running it;
`api/proto/` is a subset of its output.

## Releases

Push a tag to publish a release:

```sh
git tag v0.1.0
git push origin v0.1.0
```

CI builds linux/amd64 and linux/arm64 archives and publishes them to a GitHub
Release with `checksums.txt`. Tags with a hyphen (`v0.2.0-rc1`) become
pre-releases.
