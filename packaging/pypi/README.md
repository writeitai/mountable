# Mountable CLI

`mountable` mounts a [Mountable](https://mountable.io) filesystem on Linux or
macOS (x64 and arm64).

```sh
mkdir -p ~/mnt/work
npx -y mountable-cli mount "$FS_ID" ~/mnt/work        # npm
uvx mountable mount "$FS_ID" ~/mnt/work               # PyPI
curl -fsSL https://mountable.io/install.sh | sh         # installs to ~/.local/bin
```

`FS_ID` is a filesystem's ID, from `mountable fs list` after `mountable login`.
`mountable` is one self-contained binary. Mounting needs FUSE (`/dev/fuse`) on
Linux; on macOS there is nothing else to install and no `sudo`.
`mountable version` prints the CLI version and `mountable licenses` the
third-party licenses and notices, also shipped as `THIRD_PARTY_NOTICES`.

Documentation: https://mountable.io/docs
