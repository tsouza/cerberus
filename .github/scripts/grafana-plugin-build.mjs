// Build Metrics Drilldown from its verified upstream source and the maintained
// initialization patch. Env: GRAFANA_PLUGIN_BUILD_DIR (default /build).
// The source archive and patch are retained alongside the build for redistribution.
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';

const sourceCommit = '594c2d9bca3466f7270e5c315153d9713b4b9794';
const sourceSHA256 = '7707d3a8ffc239f3c3a205c06772dd68330bb9dffd80ee957c768358cb4595d5';
const buildDir = process.env.GRAFANA_PLUGIN_BUILD_DIR ?? '/build';
const sourceDir = path.join(buildDir, 'source');
const archivePath = path.join(buildDir, 'metrics-drilldown-source.tar.gz');
const patchPath = path.join(buildDir, 'metrics-drilldown.patch');

function run(command, args) {
  const result = spawnSync(command, args, { cwd: sourceDir, stdio: 'inherit' });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} failed (${result.signal ?? result.status})`);
}

try {
  mkdirSync(sourceDir, { recursive: true });
  const response = await fetch(`https://codeload.github.com/grafana/metrics-drilldown/tar.gz/${sourceCommit}`);
  if (!response.ok) throw new Error(`source download returned ${response.status}`);
  const archive = Buffer.from(await response.arrayBuffer());
  const actualSHA256 = createHash('sha256').update(archive).digest('hex');
  if (actualSHA256 !== sourceSHA256) throw new Error(`source archive checksum mismatch: ${actualSHA256}`);
  writeFileSync(archivePath, archive);
  run('tar', ['-xzf', archivePath, '--strip-components=1', '-C', sourceDir]);
  run('git', ['apply', '--check', patchPath]);
  run('git', ['apply', patchPath]);
  // The upstream prebuild reads git HEAD; the verified release archive has no .git.
  writeFileSync(path.join(sourceDir, 'src/version.ts'), `export const GIT_COMMIT = '${sourceCommit}';\n`);
  run('corepack', ['pnpm', 'install', '--frozen-lockfile']);
  run('corepack', ['pnpm', 'exec', 'rspack', 'build', '-c', './rspack.config.ts', '--env', 'production']);
  const plugin = JSON.parse(readFileSync(path.join(sourceDir, 'dist/plugin.json'), 'utf8'));
  if (plugin.id !== 'grafana-metricsdrilldown-app' || plugin.info.version !== '2.5.1') {
    throw new Error('built plugin identity does not match the pinned release');
  }
  console.log(`Built ${plugin.id} ${plugin.info.version} from ${sourceCommit} with the initialization guard`);
} catch (error) {
  console.error(`::error::grafana-plugin-build: ${error.message}`);
  process.exitCode = 1;
}
