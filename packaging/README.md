# Packaging the `mountable` CLI

The CLI ships as the npm package `mountable-cli`, the PyPI package
`mountable` and `https://mountable.io/install.sh`.

- `npm/`: the npm package: `bin/mountable.js` runs
  `bin/mountable-{linux,darwin}-{x64,arm64}`.
- `pypi/`: the wheel: the `mountable` entry point runs
  `mountable/bin/mountable-{linux,darwin}-{amd64,arm64}`.
- `npm-placeholder/`: `mountable-cli@0.0.0`, published once by hand (below).
- `build.sh VERSION`: builds the four binaries, the release archives
  `mountable-<os>-<arch>.tar.gz` (executable, `LICENSE`, `THIRD_PARTY_NOTICES`)
  with `SHA256SUMS`, the npm tarball and the wheel into `dist/`. The release
  workflow runs it; run it locally for a dry run.

Every channel carries `LICENSE` and `THIRD_PARTY_NOTICES`, the licenses of the
third-party code compiled into the binary (also printed by
`mountable licenses`): each module's license and NOTICE files, plus the
license headers of compiled-in source files whose copyright those files do
not carry. `go run ./tools/notices > THIRD_PARTY_NOTICES` regenerates it from
the module graph; CI fails when it is stale.

## Releasing

Push a tag `vX.Y.Z`. `.github/workflows/release.yml` builds and checks both
packages, creates the GitHub release with the archives, publishes to npm and
PyPI through trusted publishing in the `release` environment, then
smoke-tests `npx`, `uvx` and `install.sh`.

## Owner setup (once, before the first release)

1. **PyPI.** At https://pypi.org/manage/account/publishing/ add a pending
   trusted publisher: project `mountable`, owner `writeitai`, repository
   `mountable`, workflow `release.yml`, environment `release`.
2. **npm.** Publish the placeholder, signed in to the npm account that will
   own the package:

   ```sh
   npm login
   cd packaging/npm-placeholder
   npm publish --access public
   npx -y mountable-cli@0.0.0   # prints the install hint
   ```

   Then, under the package's **Settings → Trusted publishing**, add GitHub
   Actions: organization `writeitai`, repository `mountable`, workflow
   `release.yml`, environment `release`, and enable **`npm publish`** under
   Allowed actions (new publishers allow only `npm stage publish` by default).
3. **GitHub.** Create the `release` environment, restricted to tags `v*`:

   ```sh
   gh api -X PUT repos/writeitai/mountable/environments/release \
     -F 'deployment_branch_policy[protected_branches]=false' \
     -F 'deployment_branch_policy[custom_branch_policies]=true'
   gh api -X POST repos/writeitai/mountable/environments/release/deployment-branch-policies \
     -f name='v*' -f type=tag
   ```

No publish tokens are created or stored.

## The SDKs

The Python SDK (`sdk/python`, PyPI `mountable-sdk`) and the TypeScript SDK
(`sdk/typescript`, npm `mountable-sdk`) are released together by pushing a tag
`sdk-vX.Y.Z`. `.github/workflows/sdk-release.yml` tests both, stamps the
version into `pyproject.toml` and `package.json`, builds the wheel, sdist and
npm tarball, checks that they install, and publishes them through trusted
publishing in the `release` environment. CLI tags `vX.Y.Z` are unaffected.

### Owner setup (once, before the first `sdk-v` tag)

1. **PyPI.** At https://pypi.org/manage/account/publishing/ add a pending
   trusted publisher: project `mountable-sdk`, owner `writeitai`, repository
   `mountable`, workflow `sdk-release.yml`, environment `release`.
2. **npm.** Publish a placeholder `mountable-sdk@0.0.0`, signed in to the npm
   account that will own the package:

   ```sh
   npm login
   mkdir /tmp/mountable-sdk-placeholder && cd /tmp/mountable-sdk-placeholder
   npm init -y > /dev/null
   npm pkg set name=mountable-sdk version=0.0.0 license=Apache-2.0 \
     description="Placeholder for the Mountable SDK; install a released version."
   npm publish --access public
   ```

   Then, under the package's **Settings → Trusted publishing**, add GitHub
   Actions: organization `writeitai`, repository `mountable`, workflow
   `sdk-release.yml`, environment `release`, and enable **`npm publish`**
   under Allowed actions.
3. **GitHub.** Allow the `release` environment to deploy from `sdk-v*` tags
   too:

   ```sh
   gh api -X POST repos/writeitai/mountable/environments/release/deployment-branch-policies \
     -f name='sdk-v*' -f type=tag
   ```
