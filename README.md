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
`mountable mount --ticket-stdin DIR` in the sandbox. On the
first mount the CLI downloads SeaweedFS 4.48 from its official GitHub release,
checks a SHA-256 pinned in [`cmd/mountable/weed.go`](cmd/mountable/weed.go),
and caches it. Set `MOUNTABLE_WEED` to use your own `weed` binary instead.
Mounting needs FUSE (`/dev/fuse` on Linux).

Documentation: <https://mountable.io/docs>.

## Development

```sh
go test ./...
packaging/build.sh 0.0.0    # binaries, npm tarball and wheel in dist/
```

Releases are cut by pushing a tag `vX.Y.Z`; see [`packaging/README.md`](packaging/README.md).
The Mountable service itself is operated by WriteIt.ai s.r.o.; this repository
contains the open-source client only.

## License

[Apache-2.0](LICENSE).
