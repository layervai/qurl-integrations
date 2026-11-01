import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import { spawnSync } from 'node:child_process';

test.each(['mint', 'readback', 'cleanup', 'browser'])('URL-target fixture retains truthful recovery evidence after %s failure', (mode) => {
  const directory = mkdtempSync(join(tmpdir(), 'url-target-failure-'));
  const journal = join(directory, 'owned.jsonl');
  const network = join(directory, 'network.json');
  try {
    const result = spawnSync(process.execPath, [resolve(__dirname, '../node_modules/jest/bin/jest.js'),
      '--config', resolve(__dirname, '../jest.config.js'), '--runTestsByPath',
      resolve(__dirname, '../tests/url-target-one-time.test.ts'), '--runInBand', '--reporters=default',
      `--setupFilesAfterEnv=${resolve(__dirname, 'fixtures/url-target-failure.cjs')}`], {
      env: { ...process.env, QURL_NATIVE_CLEANUP_ENABLED: '1', MINT_API_URL: 'https://api.layerv.xyz/v1/qurls',
        QURL_API_KEY: 'not-a-real-key', QURL_OWNERSHIP_RECEIPTS: journal, QURL_TEST_FAILURE: mode, QURL_TEST_NETWORK: network },
      encoding: 'utf8', timeout: 15_000,
    });
    expect(result.status).toBe(1);
    const rows = readFileSync(journal, 'utf8').trim().split('\n').map((line) => JSON.parse(line));
    expect(rows.at(-1).event).toBe(mode === 'browser' ? 'owned_url_target_no_fixture' : 'owned_url_target_recovery_required');
    if (mode === 'mint' || mode === 'readback') {
      const event = mode === 'mint' ? 'owned_url_target_mint_response' : 'owned_url_target_binding_response';
      expect(rows.find((row) => row.event === event).http_status).toBe(502);
    }
    expect(JSON.parse(readFileSync(network, 'utf8'))).toEqual({ posts: mode === 'browser' ? 0 : 1,
      reads: mode === 'cleanup' || mode === 'readback' ? 1 : 0 });
  } finally { rmSync(directory, { recursive: true, force: true }); }
}, 20_000);
