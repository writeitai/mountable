#!/usr/bin/env node
// Runs the mountable binary for this platform with the same arguments,
// stdio, exit code and signals.
"use strict";
const { spawn } = require("node:child_process");
const path = require("node:path");

const target = `${process.platform}-${process.arch}`;
if (!["linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64"].includes(target)) {
  console.error(`mountable: ${target} is not supported; Mountable runs on Linux and macOS (x64, arm64).`);
  process.exit(1);
}

const child = spawn(path.join(__dirname, `mountable-${target}`), process.argv.slice(2), { stdio: "inherit" });
// `mountable mount` unmounts on SIGINT/SIGTERM, so a signal sent to this
// launcher must reach it.
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(signal, () => child.kill(signal));
child.on("error", (error) => {
  console.error(`mountable: ${error.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal) {
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
  } else {
    process.exit(code);
  }
});
