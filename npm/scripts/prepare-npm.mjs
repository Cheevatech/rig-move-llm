#!/usr/bin/env node
// Release helper (run by CI): stamp a version and lay out the per-platform npm
// packages from the cross-compiled binaries. It (1) generates
// npm/platforms/<os>-<arch>/{package.json, bin/rig-move-llm[.exe]} from the
// binaries in DIST, and (2) syncs the version into the main package.json and its
// optionalDependencies. No network, no postinstall — this is the esbuild pattern.
//
// Usage: VERSION=1.2.3 DIST=../dist node scripts/prepare-npm.mjs
import { readFileSync, writeFileSync, mkdirSync, copyFileSync, chmodSync, existsSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const npmRoot = resolve(here, "..");
const version = process.env.VERSION || "0.0.0";
const dist = resolve(process.env.DIST || join(npmRoot, "..", "dist"));

// npm --provenance requires every published package's package.json to carry a
// repository.url matching the GitHub source it was built from. Reuse the main
// package's repository so the platform packages inherit the same value.
const mainPkgPath = join(npmRoot, "package.json");
const repository = JSON.parse(readFileSync(mainPkgPath, "utf8")).repository;

// os/arch use Go's GOOS/GOARCH; cpu/nodeOS are npm's manifest fields.
const targets = [
  { os: "darwin", arch: "amd64", nodeOS: "darwin", cpu: "x64" },
  { os: "darwin", arch: "arm64", nodeOS: "darwin", cpu: "arm64" },
  { os: "linux", arch: "amd64", nodeOS: "linux", cpu: "x64" },
  { os: "linux", arch: "arm64", nodeOS: "linux", cpu: "arm64" },
  { os: "windows", arch: "amd64", nodeOS: "win32", cpu: "x64" },
  { os: "windows", arch: "arm64", nodeOS: "win32", cpu: "arm64" },
];

// npm auto-includes a LICENSE at the package root in every published tarball.
const license = join(npmRoot, "..", "LICENSE");
copyFileSync(license, join(npmRoot, "LICENSE"));

const optionalDependencies = {};
for (const t of targets) {
  const name = `@cheevatech/${t.os}-${t.arch}`;
  optionalDependencies[name] = version;

  const ext = t.os === "windows" ? ".exe" : "";
  const src = join(dist, `rig-move-llm-${t.os}-${t.arch}${ext}`);
  if (!existsSync(src)) {
    console.warn(`skip ${name}: missing binary ${src}`);
    continue;
  }
  const pkgDir = join(npmRoot, "platforms", `${t.os}-${t.arch}`);
  const binDir = join(pkgDir, "bin");
  mkdirSync(binDir, { recursive: true });

  const dst = join(binDir, `rig-move-llm${ext}`);
  copyFileSync(src, dst);
  if (!ext) chmodSync(dst, 0o755);
  copyFileSync(license, join(pkgDir, "LICENSE"));

  writeFileSync(
    join(pkgDir, "package.json"),
    JSON.stringify(
      {
        name,
        version,
        description: `rig-move-llm prebuilt binary for ${t.nodeOS}/${t.cpu}`,
        os: [t.nodeOS],
        cpu: [t.cpu],
        files: ["bin"],
        license: "MIT",
        repository,
      },
      null,
      2
    ) + "\n"
  );
  console.log(`prepared ${name} -> ${dst}`);
}

// Sync the main package version + optionalDependency versions.
const mainPath = join(npmRoot, "package.json");
const main = JSON.parse(readFileSync(mainPath, "utf8"));
main.version = version;
main.optionalDependencies = optionalDependencies;
writeFileSync(mainPath, JSON.stringify(main, null, 2) + "\n");
console.log(`stamped main package.json @ ${version}`);
