import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { appendFileSync, readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

export const packageName = '@configbutler/krm-stream';
export function releaseTags(version) {
  assert.match(version, /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/, 'expected a stable release version');
  return [`${packageName}-v${version}`, `gateway/v${version}`, `gateway/kube/v${version}`];
}

export function validateRelease(version, shas, manifest, pkg) {
  releaseTags(version);
  assert.equal(shas.length, 3);
  assert.match(shas[0], /^[a-f0-9]{40}$/);
  assert.ok(shas.every(sha => sha === shas[0]), 'release tags must identify the same commit');
  for (const path of ['packages/krm-stream', 'gateway', 'gateway/kube']) {
    assert.equal(manifest[path], version, `${path} manifest version disagrees with tag`);
  }
  assert.equal(pkg.name, packageName);
  assert.equal(pkg.version, version, 'package version disagrees with tag');
  return shas[0];
}

// Keep the outbound URL constant: manifest and workflow-input data stay local.
export async function registryMetadata(fetcher = fetch) {
  const response = await fetcher('https://registry.npmjs.org/@configbutler%2Fkrm-stream', {
    headers: { Accept: 'application/vnd.npm.install-v1+json' },
    signal: AbortSignal.timeout(30_000),
  });
  if (response.status === 404) return null;
  assert.ok(response.ok, `npm registry request failed: HTTP ${response.status}`);
  const metadata = await response.json();
  assert.equal(metadata.name, packageName);
  assert.ok(metadata.versions && typeof metadata.versions === 'object');
  assert.ok(metadata['dist-tags'] && typeof metadata['dist-tags'] === 'object');
  return metadata;
}

export async function needsPublish(version, fetcher = fetch) {
  releaseTags(version);
  const metadata = await registryMetadata(fetcher);
  if (!metadata) return true;
  const existing = metadata.versions[version];
  if (existing) {
    assert.equal(existing.version, version);
    return false;
  }
  const latest = metadata['dist-tags'].latest;
  if (latest) {
    releaseTags(latest);
    const candidate = version.split('.').map(Number);
    const current = latest.split('.').map(Number);
    const differing = candidate.findIndex((part, i) => part !== current[i]);
    assert.ok(differing >= 0 && candidate[differing] > current[differing],
      `refusing to move npm latest backwards from ${latest} to ${version}`);
  }
  return true;
}

export function validateArtifact(metadata, pkg, integrity, sha, version) {
  assert.equal(metadata.sha, sha, 'artifact was built from a different commit');
  assert.equal(metadata.integrity, integrity, 'artifact bytes disagree with build metadata');
  assert.equal(pkg.name, packageName);
  assert.equal(pkg.version, version, 'tarball version disagrees with release');
}

const git = (...args) => execFileSync('git', args, { encoding: 'utf8' }).trim();
const output = (key, value) => appendFileSync(process.env.GITHUB_OUTPUT, `${key}=${value}\n`);

async function main() {
  const version = process.env.VERSION || JSON.parse(readFileSync('.release-please-manifest.json'))['packages/krm-stream'];
  const tags = releaseTags(version);
  if (process.argv[2] === 'resolve') {
    // A package version without its tag is not a release yet. API failures must fail the job.
    const releases = JSON.parse(execFileSync('gh', ['api', '--paginate', '--slurp',
      `repos/${process.env.GITHUB_REPOSITORY}/releases`], { encoding: 'utf8' })).flat();
    const release = releases.find(item => item.tag_name === tags[0] && !item.draft && !item.prerelease);
    if (!release) {
      output('needed', false);
      console.log(`No stable GitHub release for ${version} yet.`);
      return;
    }
    const shas = tags.map(tag => git('rev-parse', `refs/tags/${tag}^{commit}`));
    const readAtTag = path => JSON.parse(git('show', `${shas[0]}:${path}`));
    const sha = validateRelease(version, shas, readAtTag('.release-please-manifest.json'),
      readAtTag('packages/krm-stream/package.json'));
    git('merge-base', '--is-ancestor', sha, 'origin/main');
    output('sha', sha);
    output('version', version);
    output('gateway_tag', tags[1]);
    output('kube_tag', tags[2]);
    output('needed', await needsPublish(version));
    console.log(`Release ${version} resolves to ${sha}.`);
  } else if (process.argv[2] === 'verify') {
    const tarball = `/tmp/npm/configbutler-krm-stream-${version}.tgz`;
    const pkg = JSON.parse(execFileSync('tar', ['-xOf', tarball, 'package/package.json'], { encoding: 'utf8' }));
    const metadata = JSON.parse(readFileSync('/tmp/npm/source.json'));
    const integrity = `sha512-${createHash('sha512').update(readFileSync(tarball)).digest('base64')}`;
    validateArtifact(metadata, pkg, integrity, process.env.RELEASE_SHA, version);
    // A retry after npm accepted the upload is successful without another publish attempt.
    output('needed', await needsPublish(version));
    output('tarball', tarball);
  } else {
    throw new Error('usage: release.mjs resolve|verify');
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main();
