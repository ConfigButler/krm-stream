import assert from 'node:assert/strict';
import { test } from 'node:test';
import { needsPublish, packageName, registryMetadata, releaseTags, validateArtifact, validateRelease } from './release.mjs';

const sha = 'a'.repeat(40);
const manifest = { 'packages/krm-stream': '0.3.0', gateway: '0.3.0', 'gateway/kube': '0.3.0' };
const pkg = { name: packageName, version: '0.3.0' };
const response = (status, body) => ({ status, ok: status === 200, json: async () => body });
const metadata = (version) => ({ name: packageName, versions: { [version]: { version } }, 'dist-tags': { latest: version } });

test('a release resolves to its tagged commit independently of the triggering commit', () => {
  assert.equal(validateRelease('0.3.0', [sha, sha, sha], manifest, pkg), sha);
  assert.throws(() => validateRelease('0.3.0', [sha, 'b'.repeat(40), sha], manifest, pkg));
  assert.throws(() => validateRelease('0.3.0', [sha, sha, sha], manifest, { ...pkg, version: '0.2.1' }));
  assert.throws(() => validateRelease('0.3.0', [sha, sha, sha], { ...manifest, gateway: '0.2.1' }, pkg));
  assert.throws(() => releaseTags('0.3.0/../../main'));
});

test('a tagged version missing from npm is recovered even without releases_created', async () => {
  const fetcher = async () => response(200, metadata('0.2.1'));
  assert.equal(await needsPublish('0.3.0', fetcher), true);
});

test('retry after successful publication is a no-op', async () => {
  assert.equal(await needsPublish('0.3.0', async () => response(200, metadata('0.3.0'))), false);
});

test('registry failures do not look like an unpublished package', async () => {
  await assert.rejects(registryMetadata(async () => response(503)));
  await assert.rejects(registryMetadata(async () => { throw new Error('network down'); }));
  await assert.rejects(registryMetadata(async () => response(200, { name: 'unexpected-package' })));
});

test('replaying an old run cannot downgrade npm latest', async () => {
  await assert.rejects(needsPublish('0.3.0', async () => response(200, metadata('0.4.0'))));
});

test('the original race and mismatched build bytes are rejected before publishing', () => {
  const metadata = { sha, integrity: 'sha512-example' };
  validateArtifact(metadata, pkg, metadata.integrity, sha, '0.3.0');
  assert.throws(() => validateArtifact(metadata, { ...pkg, version: '0.2.1' }, metadata.integrity, sha, '0.3.0'));
  assert.throws(() => validateArtifact(metadata, pkg, metadata.integrity, 'b'.repeat(40), '0.3.0'));
  assert.throws(() => validateArtifact(metadata, pkg, 'sha512-other', sha, '0.3.0'));
});

test('registry requests never include manifest or workflow-input data', async () => {
  const requests = [];
  const fetcher = async (url, options) => {
    requests.push({ url, headers: options.headers });
    return response(200, metadata('0.2.1'));
  };
  await needsPublish('0.3.0', fetcher);
  await needsPublish('0.4.0', fetcher);
  assert.deepEqual(requests[0], requests[1]);
  assert.equal(requests[0].url, 'https://registry.npmjs.org/@configbutler%2Fkrm-stream');
});
