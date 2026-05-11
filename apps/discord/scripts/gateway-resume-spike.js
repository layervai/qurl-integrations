#!/usr/bin/env node
/**
 * Cross-process Discord Gateway RESUME spike.
 *
 * Demonstrates the mechanism Pillar 2 of the zero-downtime design depends on:
 * a fresh node process can take over an existing Discord Gateway session from
 * a previous process, by reading session state (session_id, resume_url,
 * sequence) from a shared store and presenting it to @discordjs/ws's
 * `retrieveSessionInfo` callback.
 *
 * This script is NOT production code. It's a runnable validation harness.
 * Production code lands in `src/gateway-shipper/` in a later PR; this script
 * exists so a reviewer can verify the mechanism end-to-end before we sink
 * weeks into the rest of the architecture.
 *
 * Scope of this spike
 * -------------------
 *
 * Validates:
 *   - @discordjs/ws's retrieveSessionInfo / updateSessionInfo option contract
 *     is real, named correctly, and our SessionInfo shape is accepted.
 *   - Discord accepts a RESUME (op 6) from a fresh node process using session
 *     state captured by a previous process, provided the second process
 *     starts within the resume buffer window (~60s).
 *   - On resume-rejection, @discordjs/ws calls updateSessionInfo(shardId, null)
 *     and falls back to a fresh IDENTIFY without crashing.
 *
 * Does NOT validate:
 *   - DDB-backed session storage (the spike uses a local JSON file).
 *   - The hot-standby push-handoff timing (Pillar 3 — separate spike or
 *     end-to-end test once PR 13 ships).
 *   - SQS forwarding of dispatch events (Pillar 1 — covered by PR 10/11).
 *   - The leader-election lock primitive (PR 13).
 *   - Behaviour across discord.js or @discordjs/ws version bumps — the
 *     contract test in tests/gateway-resume-spike.test.js pins option names
 *     against the installed lib version, but a major bump might still need
 *     a re-spike.
 *
 * Runbook
 * -------
 *
 *   # Phase 1 — first process captures session state.
 *   # Connects, waits 10s for READY + a few heartbeats, persists state
 *   # to /tmp/spike-session.json, closes cleanly with code 1000.
 *   DISCORD_TOKEN=<sandbox bot token> \
 *     node scripts/gateway-resume-spike.js phase1
 *
 *   # Phase 2 — second process resumes from the persisted state.
 *   # Loads /tmp/spike-session.json, configures retrieveSessionInfo to
 *   # return it, opens a fresh connection. Logs the Discord op-code so
 *   # you can verify it was RESUME (op 6 reply, no IDENTIFY).
 *   DISCORD_TOKEN=<same sandbox bot token> \
 *     node scripts/gateway-resume-spike.js phase2
 *
 *   # Expected:
 *   #   Phase 1: connects, READY received, sequence advances, persists
 *   #            { session_id, resume_url, sequence }, exits cleanly.
 *   #   Phase 2: loads the state, retrieveSessionInfo returns it,
 *   #            @discordjs/ws sends RESUME (op 6). Console logs
 *   #            "RESUMED dispatch received" once Discord ACKs the resume.
 *   #            If the resume fails (session aged out, version skew),
 *   #            you'll see "updateSessionInfo(null)" and IDENTIFY fires.
 *
 * Phase 2 must run within ~60s of Phase 1 exiting — Discord buffers a
 * resumable session for around that window. Past 60s the resume is
 * rejected, @discordjs/ws falls back to IDENTIFY, and updateSessionInfo
 * is called with null to signal the failure.
 *
 * Use a SANDBOX bot token. Running against prod would briefly knock the
 * prod gateway offline (a second IDENTIFY on the same token kicks the
 * first session).
 */

const fs = require('node:fs');
const path = require('node:path');
const { WebSocketManager, WebSocketShardEvents } = require('@discordjs/ws');
const { REST } = require('@discordjs/rest');
const { GatewayIntentBits } = require('discord-api-types/v10');

const SESSION_FILE = process.env.SPIKE_SESSION_FILE || '/tmp/spike-session.json';
const PHASE1_LIFETIME_MS = 10_000;

// Minimum viable intents — Guilds only. The spike isn't exercising the
// full intent set the production bot needs; that's not its job.
const INTENTS = GatewayIntentBits.Guilds;

// Persistence helpers. Exported so the test suite covers them without
// spinning up a real Discord connection. Production code uses a DDB-
// backed store (apps/discord/docs/zero-downtime-design.md Pillar 2);
// the file-backed shape here exists because the spike runs from a
// single developer machine.
function loadSession(filePath = SESSION_FILE) {
  try {
    const raw = fs.readFileSync(filePath, 'utf8');
    const parsed = JSON.parse(raw);
    // Validate the SessionInfo shape we'll hand to retrieveSessionInfo.
    // Missing fields = malformed file = treat as no session, fall back to
    // IDENTIFY. Doesn't throw — the bot must boot even if the store is
    // wedged.
    if (
      typeof parsed?.sessionId === 'string' &&
      typeof parsed?.resumeURL === 'string' &&
      typeof parsed?.sequence === 'number'
    ) {
      return parsed;
    }
    return null;
  } catch (err) {
    if (err.code === 'ENOENT') return null;
    if (err instanceof SyntaxError) return null;
    throw err;
  }
}

function persistSession(info, filePath = SESSION_FILE) {
  // Atomic-write so a SIGKILL mid-write doesn't leave a half-written file
  // that the next phase parses as corrupt. Resolve the absolute path so
  // rename() doesn't fail across relative-cwd differences in tests.
  const abs = path.resolve(filePath);
  const tmp = `${abs}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(info, null, 2));
  fs.renameSync(tmp, abs);
}

function clearSession(filePath = SESSION_FILE) {
  try { fs.unlinkSync(filePath); } catch (_) { /* nothing to clear */ }
}

async function runPhase1(token) {
  console.log('[phase1] starting fresh — will IDENTIFY');
  clearSession();

  const rest = new REST().setToken(token);

  // In-memory session storage for the producer side. We persist the LATEST
  // info to disk before exit. Throttling-to-disk-on-every-dispatch isn't
  // necessary for the spike since exit is the only persist event we care
  // about; production code throttles to ~1Hz (see design doc).
  let latestSessionInfo = null;

  const manager = new WebSocketManager({
    token,
    intents: INTENTS,
    rest,
    retrieveSessionInfo: (shardId) => {
      console.log(`[phase1] retrieveSessionInfo(${shardId}) -> null (fresh start)`);
      return null;
    },
    updateSessionInfo: (shardId, info) => {
      // info is null on session-invalidation; non-null after READY.
      if (info) {
        console.log(`[phase1] updateSessionInfo(${shardId}) seq=${info.sequence} session=${info.sessionId.slice(0, 8)}…`);
      } else {
        console.log(`[phase1] updateSessionInfo(${shardId}) -> null (session cleared)`);
      }
      latestSessionInfo = info;
    },
  });

  manager.on(WebSocketShardEvents.Dispatch, ({ data }) => {
    // op 0 dispatch — Discord's published event stream. We don't act on
    // anything here; just observing.
    if (data.t === 'READY') {
      console.log('[phase1] READY received');
    } else if (data.t === 'RESUMED') {
      console.log('[phase1] RESUMED received (unexpected — phase1 should IDENTIFY)');
    }
  });

  manager.on(WebSocketShardEvents.Error, ({ error }) => {
    console.error('[phase1] shard error:', error.message);
  });

  await manager.connect();
  console.log('[phase1] connected; idling for', PHASE1_LIFETIME_MS, 'ms to accumulate sequence');
  await new Promise((resolve) => setTimeout(resolve, PHASE1_LIFETIME_MS));

  if (!latestSessionInfo) {
    console.error('[phase1] FAIL: no session info captured. Did READY fire?');
    await manager.destroy({ code: 1000 });
    process.exit(1);
  }

  persistSession(latestSessionInfo);
  console.log('[phase1] persisted session state to', SESSION_FILE);
  console.log('[phase1] closing WS with code 1000 (clean — session stays resumable)');

  // destroy with code 1000 (Normal Closure) preserves the session on
  // Discord's side for the resume buffer window (~60s). Any other close
  // code (4xxx INVALID_SESSION etc) invalidates the session immediately.
  await manager.destroy({ code: 1000 });
  console.log('[phase1] done. Run phase2 within ~60s to resume.');
  process.exit(0);
}

async function runPhase2(token) {
  const stored = await loadSession();
  if (!stored) {
    console.error('[phase2] FAIL: no persisted session at', SESSION_FILE);
    console.error('[phase2] Run `node scripts/gateway-resume-spike.js phase1` first.');
    process.exit(1);
  }

  console.log('[phase2] loaded persisted session:', {
    session: stored.sessionId.slice(0, 8) + '…',
    seq: stored.sequence,
    resumeURL: stored.resumeURL,
  });

  const rest = new REST().setToken(token);

  // Track whether we observed a resume vs. a fresh identify.
  let resumed = false;
  let identified = false;
  let postResumeSessionCleared = false;

  const manager = new WebSocketManager({
    token,
    intents: INTENTS,
    rest,
    retrieveSessionInfo: (shardId) => {
      console.log(`[phase2] retrieveSessionInfo(${shardId}) -> stored session`);
      // Return the persisted info. @discordjs/ws will issue op 6 RESUME
      // with these values instead of op 2 IDENTIFY.
      return stored;
    },
    updateSessionInfo: (shardId, info) => {
      if (info === null) {
        // Discord rejected the resume (most commonly: session aged out
        // past the ~60s window, or version skew). @discordjs/ws clears
        // session info and will issue a fresh IDENTIFY next.
        console.log(`[phase2] updateSessionInfo(${shardId}) -> null (RESUME REJECTED — falling back to IDENTIFY)`);
        postResumeSessionCleared = true;
      } else {
        console.log(`[phase2] updateSessionInfo(${shardId}) seq=${info.sequence} session=${info.sessionId.slice(0, 8)}…`);
      }
    },
  });

  manager.on(WebSocketShardEvents.Dispatch, ({ data }) => {
    if (data.t === 'READY') {
      // READY fires on a fresh IDENTIFY. If we see this in phase2 it
      // means the resume failed and we fell back.
      identified = true;
      console.log('[phase2] READY received (== fresh IDENTIFY, resume failed)');
    } else if (data.t === 'RESUMED') {
      resumed = true;
      console.log('[phase2] RESUMED dispatch received — cross-process resume SUCCEEDED');
    }
  });

  manager.on(WebSocketShardEvents.Error, ({ error }) => {
    console.error('[phase2] shard error:', error.message);
  });

  await manager.connect();

  // Wait long enough for either RESUMED or READY to fire.
  await new Promise((resolve) => setTimeout(resolve, 8000));

  await manager.destroy({ code: 1000 });

  if (resumed) {
    console.log('[phase2] RESULT: RESUME-OK (Pillar 2 mechanism validated)');
    process.exit(0);
  }
  if (identified && postResumeSessionCleared) {
    console.log('[phase2] RESULT: RESUME-FAIL → IDENTIFY-fallback (graceful)');
    console.log('[phase2] Cause is usually session-aged-out (>60s since phase1) or version skew.');
    console.log('[phase2] The mechanism still works in production — the design treats this as');
    console.log('[phase2] a counted SLI (resume success rate) with IDENTIFY as a safety net.');
    process.exit(0);
  }
  console.error('[phase2] RESULT: UNCLEAR — neither RESUMED nor READY observed within 8s');
  process.exit(2);
}

async function main() {
  const phase = process.argv[2];
  const token = process.env.DISCORD_TOKEN;
  if (!token) {
    console.error('DISCORD_TOKEN env var required. Use a SANDBOX bot token.');
    process.exit(1);
  }

  if (phase === 'phase1') return runPhase1(token);
  if (phase === 'phase2') return runPhase2(token);

  console.error('Usage: node scripts/gateway-resume-spike.js {phase1|phase2}');
  console.error('See file header for the runbook.');
  process.exit(1);
}

// Only run the CLI entry point when invoked directly. Imported-from-test
// loads only the exported helpers.
if (require.main === module) {
  main().catch((err) => {
    console.error('[spike] fatal:', err.stack || err.message);
    process.exit(1);
  });
}

module.exports = {
  loadSession,
  persistSession,
  clearSession,
  // Re-exported so tests can pin the default. Not load-bearing; just
  // saves a require in the test file.
  SESSION_FILE,
};
