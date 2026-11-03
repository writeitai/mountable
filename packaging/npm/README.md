# mountable.io CLI

`mountable` mounts a [mountable.io](https://mountable.io) filesystem on Linux or
macOS (x64 and arm64).

```sh
curl -fsSL https://mountable.io/install.sh | sh   # standalone binary
npx -y mountable-cli version                   # or npm
uvx mountable version                          # or PyPI
```

The installer checks the package against npm's sha512 and installs to
`~/.local/bin`. All three channels run the same CLI for Linux/macOS (x64 and
arm64). With npm or PyPI, use `npx -y mountable-cli` or `uvx mountable` in
place of `mountable` below.

Sign in with `mountable login`, then mount your filesystem:

```sh
mkdir -p ~/mnt/work
mountable mount "$FS_ID" ~/mnt/work
```

`FS_ID` is a filesystem's ID, from `mountable fs list` after `mountable login`.
`mountable` is one self-contained binary. Mounting needs FUSE (`/dev/fuse`) on
Linux; on macOS there is nothing else to install and no `sudo`.
`mountable version` prints the CLI version and `mountable licenses` the
third-party licenses and notices, also shipped as `THIRD_PARTY_NOTICES`.

Documentation: https://mountable.io/docs
