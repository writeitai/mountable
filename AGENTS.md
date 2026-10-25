# Product identity

The product is **mountable.io**, not “Mountable”. Use the full domain in public
prose, documentation titles, listing copy and PR titles/descriptions. The
operator is **WriteIt.ai s.r.o.**; it is a legal company, not the product name.

Keep technical identifiers stable: the `mountable` command, `mountable-sdk`
packages, `MOUNTABLE_*` environment variables, `writeitai/mountable` repository
and SDK class `Mountable` retain their existing spelling. This is a naming rule,
not a request to rename APIs or create new integrations.

The client is open source; the hosted service is a separate paid dependency.
Do not imply that a community example is an upstream-approved integration.

# Releases

Agents release without asking the owner, who has delegated it fully. When a
merged change should reach users (the docs rely on it, or it fixes a bug),
push the tag from `main`: `vX.Y.Z` releases the CLI (`release.yml`, npm
`mountable-cli` and PyPI `mountable`), `sdk-vX.Y.Z` releases both SDKs
(`sdk-release.yml`, `mountable-sdk` on npm and PyPI). Pick the next version
by semver from the latest tag, watch the workflow until it passes, and report
the versions published.
