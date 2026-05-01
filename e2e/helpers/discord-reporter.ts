/**
 * Custom Jest reporter that posts a per-file E2E result summary to the
 * test Discord channel after every run.
 *
 * Replaces the runtime signal previously carried by the static "embed
 * delivery" test in `tests/discord-channels.test.ts`: that test posted
 * a hardcoded `Status: Passing` embed regardless of whether other tests
 * passed, so an operator scrolling Discord could not tell at a glance
 * whether a run was actually green. This reporter posts one embed per
 * run with the real per-file rollup.
 *
 * Per-file rather than per-test: today there are ~39 tests across 4
 * files; per-test would burn the 25-field embed limit and force
 * truncation. Per-file rolls cleanly to 1 field per file with a small
 * pass/fail count, and only the failing file expands to show its
 * failing test names.
 *
 * Reporter is configured in `jest.config.ts`'s `reporters` array. It
 * runs in BOTH sandbox and prod (no env gating) — the test channel
 * routing already differs by env (sandbox = `vars.E2E_CHANNEL_ID`,
 * prod = SSM `/qurl-bot-e2e-smoke-production/CHANNEL_ID`), so each
 * env's embed lands in its own channel.
 *
 * Failure handling is deliberately defensive: a missing env var or a
 * Discord API hiccup posts a `[discord-reporter]` warning to stderr
 * but never throws. The reporter is informational; it must not turn a
 * green test run red just because Discord happens to be flaky.
 */
import * as path from 'node:path';
import type {
  AggregatedResult,
  Reporter,
  TestContext,
  TestResult,
} from '@jest/reporters';

interface EmbedField {
  name: string;
  value: string;
  inline?: boolean;
}

interface DiscordEmbed {
  title: string;
  description: string;
  color: number;
  fields: EmbedField[];
  footer?: { text: string };
  timestamp?: string;
}

// Discord embed hard limits (kept here so the truncation calls below
// read against a documented constant rather than a magic number).
//   - Field value: 1024 chars
//   - Field count: 25
//   - Footer text: 2048 chars
// We size well under the per-field cap; the per-file rollup is short
// by construction and even a fully-expanded failure block sits
// comfortably below 1024.
const FIELD_VALUE_MAX = 1024;
const FIELD_COUNT_MAX = 25;

// Show at most this many failing test titles per file. Beyond this,
// append a `… and N more` line. Five is enough to communicate scope
// without exhausting the field-value budget.
const MAX_FAILING_TESTS_PER_FILE = 5;

// Green / red Discord embed colors. Fixed integers; Discord renders
// the left-side stripe in this color.
const COLOR_GREEN = 0x2ecc71;
const COLOR_RED = 0xe74c3c;

// Sniff the env label from MINT_API_URL rather than introducing a new
// required env var. `api.layerv.ai` → prod, `api.layerv.xyz` →
// sandbox; anything else → empty string (the title omits the dash).
function envLabel(): string {
  const url = process.env.MINT_API_URL ?? '';
  if (url.includes('layerv.ai')) return 'prod';
  if (url.includes('layerv.xyz')) return 'sandbox';
  return '';
}

// GitHub Actions auto-populates `GITHUB_RUN_ID`, `GITHUB_SERVER_URL`,
// `GITHUB_REPOSITORY`, and `GITHUB_SHA` for every workflow run. When
// running locally (no GH context) the footer falls back to the suite
// name only.
function footerText(): string {
  const runId = process.env.GITHUB_RUN_ID;
  const repo = process.env.GITHUB_REPOSITORY;
  const sha = process.env.GITHUB_SHA;
  if (runId && repo) {
    const shortSha = sha ? ` · ${sha.slice(0, 7)}` : '';
    return `run #${runId}${shortSha}`;
  }
  return 'qURL E2E Test Suite';
}

function buildField(suite: TestResult): EmbedField {
  const file = path.basename(suite.testFilePath);
  const total = suite.numPassingTests + suite.numFailingTests;
  const icon = suite.numFailingTests === 0 ? '✅' : '❌';
  let value = `${icon} ${suite.numPassingTests}/${total}`;

  if (suite.numFailingTests > 0) {
    const failing = suite.testResults
      .filter((t) => t.status === 'failed')
      .slice(0, MAX_FAILING_TESTS_PER_FILE)
      .map((t) => `• ${t.title}`)
      .join('\n');
    const overflow = suite.numFailingTests - MAX_FAILING_TESTS_PER_FILE;
    const overflowLine = overflow > 0 ? `\n• … and ${overflow} more` : '';
    value = `${value}\n${failing}${overflowLine}`.slice(0, FIELD_VALUE_MAX);
  }

  return { name: file, value };
}

function buildEmbed(results: AggregatedResult): DiscordEmbed {
  const passed = results.numPassedTests;
  const failed = results.numFailedTests;
  const durationSec = ((Date.now() - results.startTime) / 1000).toFixed(1);
  const label = envLabel();
  const title = `qURL E2E Test Suite${label ? ` — ${label}` : ''}`;

  const description =
    failed === 0
      ? `✅ All ${passed} tests passed · ${durationSec}s`
      : `❌ ${failed} failed, ✅ ${passed} passed · ${durationSec}s`;

  // Stable order: input is already sorted by file path in jest's
  // results; basename ordering is deterministic across runs.
  const fields = results.testResults
    .slice(0, FIELD_COUNT_MAX)
    .map(buildField);

  // If the suite ever grows past 25 files, the truncation slice above
  // hides files past the limit. Surface that fact loudly in the
  // description so the reader doesn't get a misleadingly-small list.
  const truncated = results.testResults.length - fields.length;
  const truncatedNote = truncated > 0 ? ` (showing ${fields.length}/${results.testResults.length} files)` : '';

  return {
    title,
    description: description + truncatedNote,
    color: failed === 0 ? COLOR_GREEN : COLOR_RED,
    fields,
    footer: { text: footerText() },
    timestamp: new Date().toISOString(),
  };
}

async function postEmbed(embed: DiscordEmbed): Promise<void> {
  const token = process.env.BOT_TOKEN;
  const channelId = process.env.CHANNEL_ID;
  if (!token || !channelId) {
    // eslint-disable-next-line no-console
    console.warn(
      '[discord-reporter] BOT_TOKEN or CHANNEL_ID unset — skipping Discord post (test results NOT affected).',
    );
    return;
  }

  const res = await fetch(`https://discord.com/api/v10/channels/${channelId}/messages`, {
    method: 'POST',
    headers: {
      Authorization: `Bot ${token}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ embeds: [embed] }),
  });

  if (!res.ok) {
    const body = await res.text();
    // eslint-disable-next-line no-console
    console.warn(
      `[discord-reporter] Discord POST returned ${res.status}: ${body.slice(0, 200)} (test results NOT affected).`,
    );
  }
}

export default class DiscordReporter implements Reporter {
  // Jest constructs reporters with `(globalConfig, options)`. We don't
  // need either; declare them with underscore prefix so the linter
  // doesn't flag the unused parameters.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  constructor(_globalConfig: unknown, _reporterOptions: unknown) {}

  // `_contexts` is unused (we read everything from `results`). Jest
  // requires the parameter even when unused — drop into the leading
  // underscore convention so noUnusedParameters is happy.
  async onRunComplete(_contexts: Set<TestContext>, results: AggregatedResult): Promise<void> {
    try {
      const embed = buildEmbed(results);
      await postEmbed(embed);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      // eslint-disable-next-line no-console
      console.warn(`[discord-reporter] failed to post embed: ${msg} (test results NOT affected).`);
    }
  }

  // Required by the Reporter interface but not used here. Returning
  // null tells jest "no error from this reporter" — failing tests are
  // still surfaced via jest's own exit code, independent of this hook.
  getLastError(): Error | undefined {
    return undefined;
  }
}
