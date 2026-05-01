/**
 * Posts a per-file E2E result summary embed to the test Discord
 * channel after every run.
 *
 * Resilience contract: missing env vars / Discord errors / timeouts
 * warn under `[discord-reporter]` but never throw. Reporter is a
 * status surface, not a test gate.
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

// Discord embed limits: field value 1024, field count 25.
const FIELD_VALUE_MAX = 1024;
const FIELD_COUNT_MAX = 25;
const MAX_FAILING_TESTS_PER_FILE = 5;
const COLOR_GREEN = 0x2ecc71;
const COLOR_RED = 0xe74c3c;
const COLOR_YELLOW = 0xf1c40f;

// Discord POST timeout. Reporter must never hold CI hostage on a
// hung connection.
const POST_TIMEOUT_MS = 8_000;

// `MINT_API_URL` host disambiguates env without a new env var.
function envLabel(): string {
  const url = process.env.MINT_API_URL ?? '';
  if (url.includes('layerv.ai')) return 'prod';
  if (url.includes('layerv.xyz')) return 'sandbox';
  return '';
}

// GH Actions auto-populates these. Footer links the run page so
// operators can click through from Discord; falls back to suite
// name when running locally.
function footerText(): string {
  const runId = process.env.GITHUB_RUN_ID;
  const repo = process.env.GITHUB_REPOSITORY;
  const server = process.env.GITHUB_SERVER_URL;
  const sha = process.env.GITHUB_SHA;
  if (runId && repo && server) {
    const shortSha = sha ? ` · ${sha.slice(0, 7)}` : '';
    return `${server}/${repo}/actions/runs/${runId}${shortSha}`;
  }
  return 'qURL E2E Test Suite';
}

function buildField(suite: TestResult): EmbedField {
  const file = path.basename(suite.testFilePath);
  // `numTotalTests` includes pending/todo, not just pass+fail. Using
  // the partial sum was misleading: a `3 passes + 1 it.skip` file
  // would have rendered `✅ 3/3` despite running 4. Per claude-review.
  const ran = suite.numPassingTests + suite.numFailingTests;
  const skipped = suite.numTotalTests - ran;
  const icon = suite.numFailingTests === 0 ? '✅' : '❌';
  const skippedSuffix = skipped > 0 ? ` ⏭ ${skipped}` : '';
  let value = `${icon} ${suite.numPassingTests}/${suite.numTotalTests}${skippedSuffix}`;

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
  const total = results.numTotalTests;
  const durationSec = ((Date.now() - results.startTime) / 1000).toFixed(1);
  const label = envLabel();
  const title = `qURL E2E Test Suite${label ? ` — ${label}` : ''}`;

  // Yellow on no-tests-ran (config error / wrong matcher) so a
  // misconfigured run doesn't ship a green checkmark with zero
  // signal behind it.
  let color: number;
  let description: string;
  if (total === 0) {
    color = COLOR_YELLOW;
    description = `⚠️ No tests ran · ${durationSec}s`;
  } else if (failed === 0) {
    color = COLOR_GREEN;
    description = `✅ All ${passed} tests passed · ${durationSec}s`;
  } else {
    color = COLOR_RED;
    description = `❌ ${failed} failed, ✅ ${passed} passed (of ${total}) · ${durationSec}s`;
  }

  const fields = results.testResults
    .slice(0, FIELD_COUNT_MAX)
    .map(buildField);
  const truncated = results.testResults.length - fields.length;
  const truncatedNote = truncated > 0 ? ` (showing ${fields.length}/${results.testResults.length} files)` : '';

  return {
    title,
    description: description + truncatedNote,
    color,
    fields,
    footer: { text: footerText() },
    // Run-start time, not post time — matches what's on the GH run page.
    timestamp: new Date(results.startTime).toISOString(),
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

  // AbortController bounds the POST so a hung connection can't hold
  // CI minutes hostage past the configured timeout.
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), POST_TIMEOUT_MS);
  try {
    const res = await fetch(`https://discord.com/api/v10/channels/${channelId}/messages`, {
      method: 'POST',
      headers: {
        Authorization: `Bot ${token}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ embeds: [embed] }),
      signal: controller.signal,
    });

    if (!res.ok) {
      const body = await res.text();
      // eslint-disable-next-line no-console
      console.warn(
        `[discord-reporter] Discord POST returned ${res.status}: ${body.slice(0, 200)} (test results NOT affected).`,
      );
    }
  } catch (err) {
    const isAbort = err instanceof Error && err.name === 'AbortError';
    const msg = isAbort
      ? `timeout after ${POST_TIMEOUT_MS}ms`
      : err instanceof Error ? err.message : String(err);
    // eslint-disable-next-line no-console
    console.warn(`[discord-reporter] Discord POST ${msg} (test results NOT affected).`);
  } finally {
    clearTimeout(timer);
  }
}

export default class DiscordReporter implements Reporter {
  constructor(_globalConfig: unknown, _reporterOptions: unknown) {}

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

  // Returning `undefined` signals "no reporter-side error." Test
  // failures surface via jest's own exit code, independent of this hook.
  getLastError(): Error | undefined {
    return undefined;
  }
}
