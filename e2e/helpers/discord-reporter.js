/**
 * Posts a per-file E2E result summary embed to the test Discord
 * channel after every run.
 *
 * Plain `.js` (not `.ts`) by deliberate choice: Jest's reporter
 * loader uses Node's `require()` directly, with no `ts-jest` hook
 * applied (the preset only transforms test files). A `.ts` reporter
 * SyntaxErrors in CI on the first non-JS token. The logic is
 * straightforward enough that JSDoc types on the load-bearing inputs
 * are sufficient; converting back to TS would require either
 * pre-compiling or a Node-level loader hook, both of which are heavier
 * than the value provided.
 *
 * Resilience contract: missing env vars / Discord errors / timeouts
 * warn under `[discord-reporter]` but never throw. Reporter is a
 * status surface, not a test gate.
 */
const path = require('node:path');

// Discord embed limits: field value 1024, field count 25,
// total payload 6000. Per-field cap clamped below; total tracked
// during field construction so worst-case red runs don't 400.
const FIELD_VALUE_MAX = 1024;
const FIELD_COUNT_MAX = 25;
const TOTAL_EMBED_MAX = 6000;
// Leave 500 chars of headroom for title + description + footer +
// JSON envelope; trip the budget while building fields, not after.
const FIELDS_BUDGET = TOTAL_EMBED_MAX - 500;
const MAX_FAILING_TESTS_PER_FILE = 5;
const COLOR_GREEN = 0x2ecc71;
const COLOR_RED = 0xe74c3c;
const COLOR_YELLOW = 0xf1c40f;

// Discord POST timeout. Reporter must never hold CI hostage on a
// hung connection.
const POST_TIMEOUT_MS = 8_000;

// `MINT_API_URL` host disambiguates env without a new env var: the production
// API is on `layerv.ai`; any other non-empty host is treated as non-production.
// Reconsider if a `staging.layerv.ai`-style host ever lands.
function envLabel() {
  const url = process.env.MINT_API_URL ?? '';
  if (!url) return '';
  return url.includes('layerv.ai') ? 'prod' : 'non-prod';
}

// Discord footer text isn't auto-linkified — return the run URL for
// the description (where Discord renders it as a clickable link).
// Falls back to empty when running locally.
function runUrl() {
  const runId = process.env.GITHUB_RUN_ID;
  const repo = process.env.GITHUB_REPOSITORY;
  const server = process.env.GITHUB_SERVER_URL;
  if (runId && repo && server) {
    return `${server}/${repo}/actions/runs/${runId}`;
  }
  return '';
}

function footerText() {
  const runId = process.env.GITHUB_RUN_ID;
  const sha = process.env.GITHUB_SHA;
  if (runId) {
    const shortSha = sha ? ` · ${sha.slice(0, 7)}` : '';
    return `run #${runId}${shortSha}`;
  }
  return 'qURL E2E Test Suite';
}

/**
 * @param {{ testFilePath: string, numPassingTests: number, numFailingTests: number, numTotalTests: number, testResults: Array<{ status: string, title: string }> }} suite
 * @returns {{ name: string, value: string }}
 */
function buildField(suite) {
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

/**
 * @param {{ numPassedTests: number, numFailedTests: number, numTotalTests: number, startTime: number, testResults: Array }} results
 */
function buildEmbed(results) {
  const passed = results.numPassedTests;
  const failed = results.numFailedTests;
  const total = results.numTotalTests;
  const durationSec = ((Date.now() - results.startTime) / 1000).toFixed(1);
  const label = envLabel();
  const title = `qURL E2E Test Suite${label ? ` — ${label}` : ''}`;

  // Yellow when nothing ran AND when everything was skipped: in both
  // cases the suite produced zero pass/fail signal, so a green check
  // would be misleading. Green requires `passed > 0 && failed === 0`.
  // Skipped count surfaces in the description suffix so the top-line
  // matches the per-file `⏭ N` display below it.
  const skipped = total - passed - failed;
  const skippedSuffix = skipped > 0 ? ` (${skipped} skipped)` : '';
  let color;
  let description;
  if (total === 0) {
    color = COLOR_YELLOW;
    description = `⚠️ No tests ran · ${durationSec}s`;
  } else if (passed === 0 && failed === 0) {
    color = COLOR_YELLOW;
    description = `⚠️ All ${total} tests skipped · ${durationSec}s`;
  } else if (failed === 0) {
    color = COLOR_GREEN;
    description = `✅ All ${passed} tests passed${skippedSuffix} · ${durationSec}s`;
  } else {
    color = COLOR_RED;
    description = `❌ ${failed} failed, ✅ ${passed} passed${skippedSuffix} (of ${total}) · ${durationSec}s`;
  }

  // Build fields under the 6000-char total embed budget. Worst-case red
  // run with 25 files × ~1024-char failure expansions blows the cap and
  // Discord 400s the whole POST. Stop adding fields once the running
  // total crosses FIELDS_BUDGET; surface the cutoff in the title-line
  // truncation note. Test-titles flow into embed values unescaped:
  // they originate in our own test code, not user input — Discord
  // embeds don't render markdown the way messages do, so this is the
  // intended trust boundary.
  const fields = [];
  let usedChars = title.length + description.length + footerText().length;
  for (const suite of results.testResults) {
    if (fields.length >= FIELD_COUNT_MAX) break;
    const field = buildField(suite);
    const fieldChars = field.name.length + field.value.length;
    if (usedChars + fieldChars > FIELDS_BUDGET) break;
    fields.push(field);
    usedChars += fieldChars;
  }
  const truncated = results.testResults.length - fields.length;
  const truncatedNote = truncated > 0 ? ` (showing ${fields.length}/${results.testResults.length} files)` : '';

  // Run URL belongs in the description, not the footer — Discord
  // doesn't linkify footer text.
  const url = runUrl();
  const linkLine = url ? `\n[View run on GitHub](${url})` : '';

  return {
    title,
    description: description + truncatedNote + linkLine,
    color,
    fields,
    footer: { text: footerText() },
    // Run-start time, not post time — matches what's on the GH run page.
    timestamp: new Date(results.startTime).toISOString(),
  };
}

async function postEmbed(embed) {
  const token = process.env.BOT_TOKEN;
  const channelId = process.env.CHANNEL_ID;
  if (!token || !channelId) {
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
      console.warn(
        `[discord-reporter] Discord POST returned ${res.status}: ${body.slice(0, 200)} (test results NOT affected).`,
      );
    }
  } catch (err) {
    const isAbort = err instanceof Error && err.name === 'AbortError';
    const msg = isAbort
      ? `timeout after ${POST_TIMEOUT_MS}ms`
      : err instanceof Error ? err.message : String(err);
    console.warn(`[discord-reporter] Discord POST ${msg} (test results NOT affected).`);
  } finally {
    clearTimeout(timer);
  }
}

class DiscordReporter {
  constructor(_globalConfig, _reporterOptions) {}

  async onRunComplete(_contexts, results) {
    try {
      const embed = buildEmbed(results);
      await postEmbed(embed);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      console.warn(`[discord-reporter] failed to post embed: ${msg} (test results NOT affected).`);
    }
  }

  // Returning `undefined` signals "no reporter-side error." Test
  // failures surface via jest's own exit code, independent of this hook.
  getLastError() {
    return undefined;
  }
}

module.exports = DiscordReporter;
