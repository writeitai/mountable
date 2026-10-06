# Mountable CLI

`mountable` mounts a [Mountable](https://mountable.io) filesystem, a shared
POSIX filesystem for agents and teams, on any Linux or macOS machine or agent
sandbox.

```sh
npx -y mountable-cli login                          # npm
uvx mountable login                                 # PyPI
curl -fsSL https://mountable.io/install.sh | sh     # standalone binary
```

People sign in with `mountable login` and mount with
`mountable mount FILESYSTEM_ID DIR`. Agents get a one-time ticket from their
backend, which holds a Mountable API key, and run
`mountable mount --ticket-stdin DIR` in the sandbox. The CLI is one
self-contained binary: it downloads nothing at mount time and keeps the
session's credentials in memory. Mounting needs FUSE (`/dev/fuse` on Linux).

`mountable version` prints the CLI version; `mountable licenses` prints the
licenses and notices of the third-party code compiled into it, which also ship
as [`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES) with every package and release
archive.

Documentation: <https://mountable.io/docs>.

## Development

```sh
go test ./...
go run ./tools/notices > THIRD_PARTY_NOTICES   # after changing dependencies
packaging/build.sh 0.0.0    # binaries, release archives, npm tarball and wheel in dist/
```

Releases are cut by pushing a tag `vX.Y.Z`; see [`packaging/README.md`](packaging/README.md).
The Mountable service itself is operated by WriteIt.ai s.r.o.; this repository
contains the open-source client only.

## License

[Apache-2.0](LICENSE).
