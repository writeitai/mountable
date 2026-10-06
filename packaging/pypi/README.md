# Mountable CLI

`mountable` mounts a [Mountable](https://mountable.io) filesystem on Linux or
macOS (x64 and arm64).

```sh
npx -y mountable-cli mount <filesystem> /mnt/work      # npm
uvx mountable mount <filesystem> /mnt/work             # PyPI
curl -fsSL https://mountable.io/install.sh | sh         # installs to ~/.local/bin
```

`mountable` is one self-contained binary; mounting needs FUSE (`/dev/fuse` on
Linux). `mountable version` prints the CLI version and `mountable licenses` the
third-party licenses and notices, also shipped as `THIRD_PARTY_NOTICES`.

Documentation: https://mountable.io/docs
