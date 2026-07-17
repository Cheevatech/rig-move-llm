#!/usr/bin/env node
// Thin launcher: resolve the prebuilt binary for this platform (delivered as an
// optionalDependency @rig-move-llm/<os>-<arch>, the esbuild/biome pattern — no
// postinstall download) and hand off argv verbatim.
"use strict";

const path = require("path");
const { spawnSync } = require("child_process");

// Map Node's identifiers to our Go-style GOOS/GOARCH package suffixes.
const OS = { darwin: "darwin", linux: "linux", win32: "windows" };
const ARCH = { x64: "amd64", arm64: "arm64" };

function binaryPath() {
  const goos = OS[process.platform];
  const goarch = ARCH[process.arch];
  if (!goos || !goarch) {
    fail(`unsupported platform: ${process.platform}/${process.arch}`);
  }
  const pkg = `@rig-move-llm/${goos}-${goarch}`;
  const ext = goos === "windows" ? ".exe" : "";
  try {
    // Resolve via the platform package's package.json, then join to its binary.
    const pkgJson = require.resolve(`${pkg}/package.json`);
    return path.join(path.dirname(pkgJson), "bin", `rig-move-llm${ext}`);
  } catch (_) {
    fail(
      `missing platform package ${pkg}.\n` +
        `Reinstall with optional dependencies enabled (npm install --include=optional),\n` +
        `or grab a binary from https://github.com/rigmovellm/rig-move-llm/releases`
    );
  }
}

function fail(msg) {
  process.stderr.write(`rig-move-llm: ${msg}\n`);
  process.exit(1);
}

const result = spawnSync(binaryPath(), process.argv.slice(2), { stdio: "inherit" });
if (result.error) fail(result.error.message);
process.exit(result.status === null ? 1 : result.status);
