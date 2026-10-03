import assert from 'node:assert/strict';
import { test } from 'node:test';
import { needsPublish, packageName, publishWithRecovery, registryMetadata, releaseTags, validatePackage, validateRelease } from './release.mjs';

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

test('an older tarball or the wrong package cannot be published as the release', () => {
  validatePackage(pkg, '0.3.0');
  assert.throws(() => validatePackage({ ...pkg, version: '0.2.1' }, '0.3.0'));
  assert.throws(() => validatePackage({ ...pkg, name: 'krm-stream' }, '0.3.0'));
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

const integrity = `sha512-${Buffer.alloc(64, 1).toString('base64')}`;
const duplicate = () => Object.assign(new Error('npm failed'), {
  stderr: 'npm error 403 You cannot publish over the previously published versions: 0.3.0.',
});
const publishedMetadata = (value = integrity) => ({ ...metadata('0.3.0'),
  versions: { '0.3.0': { version: '0.3.0', dist: { integrity: value } } } });

test('a successful publish does not require another registry read', async () => {
  assert.equal(await publishWithRecovery({ version: '0.3.0', integrity,
    publish: async () => {}, fetcher: async () => assert.fail('unexpected registry read') }), 'published');
});

test('duplicate upload recovers only after matching metadata becomes visible, without republishing', async () => {
  let publishes = 0;
  let reads = 0;
  const waits = [];
  const result = await publishWithRecovery({ version: '0.3.0', integrity,
    publish: async () => { publishes++; throw duplicate(); },
    fetcher: async () => response(200, ++reads === 1 ? metadata('0.2.1') : publishedMetadata()),
    wait: async ms => waits.push(ms) });
  assert.equal(result, 'already published');
  assert.equal(publishes, 1);
  assert.equal(reads, 2);
  assert.deepEqual(waits, [2_000]);
});

test('duplicate publication never accepts different or missing integrity', async () => {
  for (const value of ['sha512-different', undefined]) {
    await assert.rejects(publishWithRecovery({ version: '0.3.0', integrity,
      publish: async () => { throw duplicate(); },
      fetcher: async () => response(200, publishedMetadata(value === undefined ? null : value)) }),
    /does not match/);
  }
});

test('authentication and other publication errors remain failures', async () => {
  for (const stderr of ['npm error E403 access denied', 'npm error ECONNRESET',
    'You cannot publish over the previously published versions: 0.2.1.']) {
    const error = Object.assign(new Error('publish failed'), { stderr });
    await assert.rejects(publishWithRecovery({ version: '0.3.0', integrity,
      publish: async () => { throw error; }, fetcher: async () => assert.fail('unexpected registry read') }),
    caught => caught === error);
  }
});

test('unconfirmed duplicate publication has bounded reads and remains a failure', async () => {
  let reads = 0;
  const error = duplicate();
  await assert.rejects(publishWithRecovery({ version: '0.3.0', integrity,
    publish: async () => { throw error; },
    fetcher: async () => { reads++; return response(404); }, wait: async () => {} }),
  caught => caught === error);
  assert.equal(reads, 5);
});

test('registry outage during duplicate confirmation remains a failure', async () => {
  await assert.rejects(publishWithRecovery({ version: '0.3.0', integrity,
    publish: async () => { throw duplicate(); }, fetcher: async () => response(503) }), /HTTP 503/);
});
