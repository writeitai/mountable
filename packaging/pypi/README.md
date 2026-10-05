# Mountable CLI

`mountable` mounts a [Mountable](https://mountable.io) filesystem on Linux or
macOS (x64 and arm64).

```sh
npx -y mountable-cli mount <filesystem> /mnt/work      # npm
uvx mountable mount <filesystem> /mnt/work             # PyPI
curl -fsSL https://mountable.io/install.sh | sh         # installs to ~/.local/bin
```

Mounting needs FUSE (`/dev/fuse` on Linux). `mountable version` prints the CLI
and SeaweedFS versions.

Documentation: https://mountable.io/docs
