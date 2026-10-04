import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { goReleaseReady, needsPublish, packageName, publishWithRecovery, registryMetadata, releaseTags, validatePackage, validateRelease, waitForGoRelease } from './release.mjs';

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

const goResponse = (status, body = '') => ({ status, ok: status === 200, text: async () => body });
const missingGoTag = (path = 'gateway', version = '0.3.0') =>
  `not found: github.com/ConfigButler/krm-stream/${path}@v${version}: invalid version: unknown revision ${path}/v${version}`;

test('Go readiness CLI requires an explicit release version before reading any local manifest', () => {
  const env = { ...process.env };
  delete env.VERSION;
  const result = spawnSync(process.execPath, [fileURLToPath(new URL('./release.mjs', import.meta.url)), 'go-ready'], {
    env, cwd: tmpdir(), encoding: 'utf8', timeout: 5_000,
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /go-ready requires an explicit VERSION from release preparation/);
});

test('Go readiness checks both release modules and validates the version before any request', async () => {
  const urls = [];
  const fetcher = async url => { urls.push(url); return goResponse(200); };
  assert.equal(await goReleaseReady('0.3.0', fetcher), true);
  assert.deepEqual(urls, [
    'https://sum.golang.org/lookup/github.com/!config!butler/krm-stream/gateway@v0.3.0',
    'https://sum.golang.org/lookup/github.com/!config!butler/krm-stream/gateway/kube@v0.3.0',
  ]);
  await assert.rejects(goReleaseReady('0.3.0/../../main', async () => assert.fail('unexpected request')));
});

test('only an exact missing-tag response for the requested module is a propagation delay', async () => {
  for (const path of ['gateway', 'gateway/kube']) {
    for (const status of [404, 410]) {
      assert.equal(await goReleaseReady('0.3.0', async url => url.endsWith(`/${path}@v0.3.0`)
        ? goResponse(status, missingGoTag(path)) : goResponse(200)), false);
    }
  }
  for (const [status, body] of [
    [404, ''], [404, missingGoTag('gateway', '0.2.1')], [404, missingGoTag('unrelated')],
    [404, `${missingGoTag()}\nchecksum mismatch`], [403, missingGoTag()], [503, missingGoTag()],
  ]) {
    await assert.rejects(goReleaseReady('0.3.0', async () => goResponse(status, body)), /Go checksum lookup/);
  }
});

test('a missing tag in one module cannot hide an unrelated error in the other', async () => {
  for (const missingPath of ['gateway', 'gateway/kube']) {
    await assert.rejects(waitForGoRelease('0.3.0', {
      fetcher: async url => url.endsWith(`/${missingPath}@v0.3.0`)
        ? goResponse(404, missingGoTag(missingPath)) : goResponse(503, 'service unavailable'),
      wait: async () => assert.fail('unrelated errors must not wait'),
    }), /HTTP 503/);
  }
  await assert.rejects(waitForGoRelease('0.3.0', {
    fetcher: async () => { throw new Error('network down'); },
    wait: async () => assert.fail('network errors must not wait'),
  }), /network down/);
});

test('release preparation proceeds as soon as both checksum records are visible', async () => {
  let reads = 0;
  const waits = [];
  await waitForGoRelease('0.3.0', {
    fetcher: async () => ++reads === 1 ? goResponse(404, missingGoTag()) : goResponse(200),
    wait: async ms => waits.push(ms),
  });
  assert.equal(reads, 4);
  assert.deepEqual(waits, [60_000]);
});

test('ready releases do not wait', async () => {
  await waitForGoRelease('0.3.0', {
    fetcher: async () => goResponse(200),
    wait: async () => assert.fail('ready records must not wait'),
  });
});

test('the readiness deadline includes request time and never waits beyond the remaining budget', async () => {
  let time = 0;
  const waits = [];
  await assert.rejects(waitForGoRelease('0.3.0', {
    now: () => time,
    fetcher: async url => {
      time += 7_000;
      return goResponse(404, missingGoTag(url.includes('/gateway/kube@') ? 'gateway/kube' : 'gateway'));
    },
    wait: async ms => { waits.push(ms); time += ms; },
  }), /still missing after 30 minutes/);
  assert.ok(waits.every(ms => ms > 0 && ms <= 60_000));
  assert.ok(waits.at(-1) < 60_000);
  assert.ok(waits.reduce((sum, ms) => sum + ms, 0) < 30 * 60_000);
});
