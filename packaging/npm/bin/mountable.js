#!/usr/bin/env node
// Runs the mountable binary for this platform with the same arguments,
// stdio, exit code and signals.
"use strict";
const { spawn } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const target = `${process.platform}-${process.arch}`;
const binary = path.join(__dirname, `mountable-${target}${process.platform === "win32" ? ".exe" : ""}`);
const supported = ["linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64", "win32-x64", "win32-arm64"];
if (!supported.includes(target) || !fs.existsSync(binary)) {
  console.error(`mountable: ${target} is not supported by this package; mountable.io runs on Linux and macOS (x64, arm64).`);
  process.exit(1);
}

const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });
if (process.platform === "win32") {
  // Ctrl-C and closing the console reach the binary directly, as it shares
  // this console; killing it instead would end a mount without unmounting.
  // This launcher only waits for it.
  for (const signal of ["SIGINT", "SIGBREAK", "SIGHUP"]) process.on(signal, () => {});
} else {
  // `mountable mount` unmounts on SIGINT/SIGTERM, so a signal sent to this
  // launcher must reach it.
  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(signal, () => child.kill(signal));
}
child.on("error", (error) => {
  console.error(`mountable: ${error.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal && process.platform !== "win32") {
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
  } else {
    process.exit(code ?? 1);
  }
});
