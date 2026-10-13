import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {dirname, resolve} from 'node:path';
import test from 'node:test';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const workflow = readFileSync(resolve(root, '.github/workflows/release.yml'), 'utf8');
const config = JSON.parse(readFileSync(resolve(root, 'release-please-config.json'), 'utf8'));
const source = workflow.split('          (async () => {\n')[1].split('          })().catch')[0];
const execute = new (Object.getPrototypeOf(async function () {}).constructor)(
  'require', 'process', 'fetch', 'AbortSignal', 'Date', 'setTimeout', source,
);
const sha = 'a'.repeat(40);
const other = 'b'.repeat(40);
const tag = 'v1.0.0';
const ci = (overrides = {}) => ({id: 9, head_sha: sha, event: 'push', head_branch: 'main', status: 'completed', conclusion: 'success', ...overrides});

async function scenario(options = {}) {
  const calls = [];
  let output = '';
  let tick = 0;
  let reads = 0;
  let polls = 0;
  let ref = options.ref;
  const release = {id: 42, target_commitish: sha, tag_name: tag, draft: true, ...options.release};
  const env = {GH_TOKEN: 'app', CI_TOKEN: 'ci', GITHUB_REPOSITORY: 'Hushscript/sdk', RELEASE_ID: '42', COMMIT: sha, TAG: tag, GITHUB_OUTPUT: '/unused', ...options.env};
  async function fetch(url, request) {
    const path = url.replace('https://api.github.com/repos/Hushscript/sdk/', '');
    const body = request.body && JSON.parse(request.body);
    calls.push({path, method: request.method, body, token: request.headers.Authorization});
    let data;
    let status = 200;
    if (path === options.apiFailure) status = 403;
    else if (path === 'releases/42' && request.method === 'GET') {
      reads++;
      data = {...release, ...(reads >= (options.changeAt ?? Infinity) ? options.change : {})};
    } else if (path.startsWith('contents/')) {
      assert.equal(path, `contents/.release-please-manifest.json?ref=${sha}`);
      data = {content: Buffer.from(JSON.stringify({'.': options.version ?? '1.0.0'})).toString('base64')};
    } else if (path === `compare/${sha}...main`) data = {status: options.ancestry ?? 'ahead'};
    else if (path.startsWith('actions/')) {
      assert.equal(path, `actions/workflows/ci.yml/runs?event=push&head_sha=${sha}&per_page=100`);
      assert.equal(request.headers.Authorization, 'Bearer ci');
      polls++;
      data = {workflow_runs: options.runs ?? (options.wait && polls === 1 ? [ci({status: 'in_progress'})] : [ci()])};
    } else if (path === `git/ref/tags/${tag}`) {
      if (!ref) status = 404;
      else data = ref;
    } else if (path === 'git/refs') {
      assert.deepEqual(body, {ref: `refs/tags/${tag}`, sha});
      ref = {ref: `refs/tags/${tag}`, object: {type: 'commit', sha: options.createdSha ?? sha}};
      if (options.concurrent) status = 422;
    } else if (path === 'releases/42' && request.method === 'PATCH') {
      assert.deepEqual(body, {draft: false, tag_name: tag, target_commitish: sha});
      data = {...release, ...body, ...options.published};
    } else throw new Error(`Unexpected API ${request.method} ${path}`);
    return {status, ok: status >= 200 && status < 300, json: async () => data};
  }
  let error;
  try {
    await execute(() => ({appendFileSync: (_path, value) => {output += value;}}), {env}, fetch,
      {timeout: () => undefined}, {now: () => (tick += options.timeout ? 2000000 : 1)}, resolve => resolve());
  } catch (caught) {error = caught;}
  return {calls, output, error};
}

const writes = result => result.calls.filter(call => call.method !== 'GET');
const stored = (target = sha, type = 'commit') => ({ref: `refs/tags/${tag}`, object: {type, sha: target}});

test('lint and ci enforce the release regressions in the pinned offline container', () => {
  const recipes = readFileSync(resolve(root, 'justfile'), 'utf8');
  assert.match(recipes, /node_image := "node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392"/);
  const lint = recipes.split('lint version=floor:')[1].split('\n\n')[0];
  assert.match(lint, /run\(\) \{ docker run .*--network none/);
  assert.match(lint, /run --label codex.adhoc=1 \{\{node_image\}\} timeout 60 node --test scripts\/release-candidate.test.mjs/);
  assert.match(recipes.split('ci *versions:')[1] ?? recipes.split('ci version=')[1] ?? recipes, /for v in \$versions; do\s+just lint "\$v"/);
  if (root.endsWith('hushscript-sdk-php')) {
    const attributes = readFileSync(resolve(root, '.gitattributes'), 'utf8');
    assert.match(attributes, /^\/scripts export-ignore$/m);
    assert.match(attributes, /^\/justfile export-ignore$/m);
  }
});

test('draft config and protected exact-candidate wiring', () => {
  assert.equal(config.draft, true);
  assert.equal(config['force-tag-creation'], false);
  assert.equal(config['initial-version'], '1.0.0');
  assert.equal(config['include-component-in-tag'], false);
  assert.doesNotMatch(workflow, /^          release-type:/m);
  assert.match(workflow, /environment: release/);
  assert.match(workflow, /COMMIT: \$\{\{ needs.release-please.outputs.sha \}\}/);
  assert.match(workflow, /RELEASE_ID: \$\{\{ needs.release-please.outputs.id \}\}/);
  assert.doesNotMatch(workflow, /github.sha/);
  if (root.endsWith('hushscript-sdk-go')) {
    const warm = workflow.split('  warm-proxy:')[1];
    assert.match(warm, /needs: publish-release/);
    assert.match(warm, /if: needs.publish-release.outputs.release_created == 'true'/);
  }
});

for (const [name, options] of [
  ['delayed workflow checks selected release SHA, not triggering SHA', {}],
  ['waits for running candidate CI', {wait: true}],
  ['accepts existing exact lightweight tag for failed-job retry', {ref: stored()}],
  ['verifies tag after concurrent creation conflict', {concurrent: true}],
]) test(name, async () => {
  const result = await scenario(options);
  assert.equal(result.error, undefined);
  assert.equal(result.output, `release_created=true\ntag_name=${tag}\n`);
  const firstWrite = result.calls.findIndex(call => call.method !== 'GET');
  const lastCi = result.calls.findLastIndex(call => call.path.startsWith('actions/'));
  assert.ok(firstWrite > lastCi);
  assert.equal(writes(result).at(-1).path, 'releases/42');
  assert.ok(result.calls.filter(call => call.path === 'releases/42' && call.method === 'GET').length >= 3);
});

for (const [name, options] of [
  ['failed CI', {runs: [ci({conclusion: 'failure'})]}],
  ['cancelled CI', {runs: [ci({conclusion: 'cancelled'})]}],
  ['newest CI failure overrides older success', {runs: [ci({id: 1}), ci({id: 2, conclusion: 'failure'})]}],
  ['wrong SHA success cannot authorize release', {runs: [ci({head_sha: other})], timeout: true}],
  ['PR success cannot authorize release', {runs: [ci({event: 'pull_request'})], timeout: true}],
  ['wrong branch success cannot authorize release', {runs: [ci({head_branch: 'other'})], timeout: true}],
  ['missing CI times out', {runs: [], timeout: true}],
  ['draft id changed', {release: {id: 43}}],
  ['draft target is mutable branch', {release: {target_commitish: 'main'}}],
  ['draft SHA changed', {release: {target_commitish: other}}],
  ['draft tag changed', {release: {tag_name: 'v0.2.0'}}],
  ['release already public', {release: {draft: false}}],
  ['manifest mismatch', {version: '0.2.0'}],
  ['candidate outside main', {ancestry: 'diverged'}],
  ['draft changed during wait', {changeAt: 2, change: {target_commitish: other}}],
  ['CI API failure fails closed', {apiFailure: `actions/workflows/ci.yml/runs?event=push&head_sha=${sha}&per_page=100`}],
  ['invalid frozen SHA', {env: {COMMIT: 'main'}}],
  ['invalid release id', {env: {RELEASE_ID: '../42'}}],
  ['invalid tag', {env: {TAG: 'v1.0.0/other'}}],
]) test(`blocks before any tag: ${name}`, async () => {
  const result = await scenario(options);
  assert.ok(result.error);
  assert.deepEqual(writes(result), []);
  assert.equal(result.output, '');
});

for (const [name, options] of [
  ['conflicting existing tag', {ref: stored(other)}],
  ['annotated tag is rejected', {ref: stored(sha, 'tag')}],
  ['created tag readback mismatch', {createdSha: other}],
  ['draft changes after tag verification', {changeAt: 3, change: {tag_name: 'v0.2.0'}}],
]) test(`blocks publication: ${name}`, async () => {
  const result = await scenario(options);
  assert.ok(result.error);
  assert.ok(!writes(result).some(call => call.method === 'PATCH'));
  assert.equal(result.output, '');
});

test('publication response mismatch blocks downstream warm-up', async () => {
  const result = await scenario({published: {id: 43}});
  assert.ok(result.error);
  assert.equal(result.output, '');
});

test('ci runs every runtime test stage while retaining a failed security gate', () => {
  const recipes = readFileSync(resolve(root, 'justfile'), 'utf8');
  const body = recipes.split('ci version="":')[1].split('\n\n')[0]
    .split('\n').filter(line => !line.includes('{{'))
    .map(line => line.replace(/^    /, '')).join('\n');
  const script = `versions="1.25 1.26 1.27"
just() {
  printf '%s %s\n' "$1" "$2"
  if [ "$1" = lint ] && [ "$2" = 1.25 ]; then return 3; fi
  return 0
}
${body}`;
  const result = spawnSync('bash', ['-c', script], {encoding: 'utf8', timeout: 5000});
  assert.equal(result.error, undefined);
  assert.equal(result.status, 3);
  assert.deepEqual(result.stdout.trim().split('\n'), ['1.25', '1.26', '1.27'].flatMap(version =>
    ['lint', 'test', 'conformance', 'examples', 'build'].map(recipe => `${recipe} ${version}`)));
});
