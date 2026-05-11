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
 *   # to $TMPDIR/qurl-spike-$UID/spike-session.json (owner-only dir +
 *   # owner-only file), closes cleanly with code 1000.
 *   DISCORD_TOKEN=<sandbox bot token> \
 *     node scripts/gateway-resume-spike.js phase1
 *
 *   # Phase 2 — second process resumes from the persisted state.
 *   # Loads the same path, configures retrieveSessionInfo to return it,
 *   # opens a fresh connection. Logs the Discord op-code so you can
 *   # verify it was RESUME (op 6 reply, no IDENTIFY).
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
 *
 * Exit codes
 * ----------
 *
 *   0 — Success. Either RESUME-OK (phase2 saw RESUMED dispatch) or
 *       graceful RESUME-FAIL → IDENTIFY-fallback (phase2 saw READY
 *       after Discord cleared the session). Both indicate the mechanism
 *       is working; the resume-success SLI is what differs.
 *   1 — Internal error. Bad token, missing persisted session in phase2,
 *       persistSession threw from a signal handler. See stderr for the
 *       specific failure.
 *   2 — UNCLEAR. Phase2 connected (or attempted to) but neither RESUMED
 *       nor READY observed within the timeout. Usually a Discord-side
 *       issue or a connect() hang the global timeout didn't catch.
 *   3 — Token contention. RESUME rejected AND IDENTIFY budget exhausted.
 *       Almost always means another process is IDENTIFYing on the same
 *       token; scale that process down before re-running the spike.
 *   130 — phase1 SIGINT (user Ctrl+C). Partial session was persisted if
 *         READY had already fired; otherwise nothing to persist.
 *   143 — phase1 SIGTERM (kill <pid>). Same behaviour as 130, distinct
 *         code so a wrapping script can tell user-cancel from external
 *         termination.
 */

const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { WebSocketManager, WebSocketShardEvents } = require('@discordjs/ws');
const { REST } = require('@discordjs/rest');
const { GatewayIntentBits } = require('discord-api-types/v10');

// Per-user, owner-only directory under tmpdir for the session file.
// Plain /tmp/spike-session.json was exploitable in a narrow but real
// way: between persistSession's unlinkSync(tmp) and the subsequent
// writeFileSync, anyone with write access to /tmp could create the
// path as a symlink to an arbitrary target — writeFileSync follows
// symlinks, so the secret would land at the attacker's target under
// 0o600. Locating the file inside a 0o700 directory owned by the
// invoking user closes that vector at the directory layer.
//
// Resolved at module load (cheap; idempotent). The mkdirSync is
// safe to call when the directory already exists (`recursive: true`).
const SPIKE_DIR = path.join(os.tmpdir(), `qurl-spike-${os.userInfo().uid}`);
fs.mkdirSync(SPIKE_DIR, { recursive: true, mode: 0o700 });
// chmodSync as belt-and-suspenders against a pre-existing directory
// that was created with looser perms (mkdirSync's `mode` is ignored
// if the directory already exists).
fs.chmodSync(SPIKE_DIR, 0o700);

const SESSION_FILE = process.env.SPIKE_SESSION_FILE || path.join(SPIKE_DIR, 'spike-session.json');
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
  //
  // Secret-on-disk shape: the persisted file contains a live Discord
  // session_id + resume_url that, within the ~60s resume buffer window,
  // anyone with read access could use to RESUME the bot's WS session.
  // Even on a personal dev machine, world-readable in /tmp is sloppy
  // hygiene. We close the secrets-at-rest window in three steps:
  //
  //   1. Best-effort unlink of any pre-existing .tmp BEFORE writing.
  //      This forces writeFileSync down the file-creation path, where
  //      its `mode: 0o600` argument is actually honored. Without the
  //      unlink, a stale .tmp from a crashed previous run takes the
  //      open-existing path and writeFileSync silently ignores `mode`
  //      — so the secret would briefly land on disk under whatever
  //      mode the stale .tmp carried (commonly 0o644).
  //   2. mode: 0o600 on the create. Owner-only read/write.
  //   3. chmodSync(0o600) after the write as belt-and-suspenders for
  //      the corner case where the unlink loses a race with another
  //      process recreating .tmp between unlinkSync and writeFileSync.
  //      (Vanishingly unlikely on a dev machine; cheap to keep.)
  const abs = path.resolve(filePath);
  const tmp = `${abs}.tmp`;
  try { fs.unlinkSync(tmp); } catch (e) {
    if (e.code !== 'ENOENT') throw e; // any non-missing-file error is a real problem
  }
  fs.writeFileSync(tmp, JSON.stringify(info, null, 2), { mode: 0o600 });
  fs.chmodSync(tmp, 0o600);
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

  // SIGINT and SIGTERM both persist captured state before exit so phase2
  // has a session to RESUME against. Exit codes (130 / 143) follow shell
  // convention so a calling script can distinguish user-cancel from
  // external-term.
  let exiting = false;
  const persistAndExit = (signal) => {
    if (exiting) return; // debounce: second signal after persist-but-before-exit
    exiting = true;
    // try/catch so persistSession throwing (disk-full, chmodSync EACCES,
    // ENOENT on rename) doesn't strand us in the signal handler. Once
    // `exiting=true`, our registered handler eats subsequent signals, and
    // a thrown error would skip process.exit entirely — leaving phase1
    // running with no obvious way to kill it short of SIGKILL.
    try {
      if (latestSessionInfo) {
        persistSession(latestSessionInfo);
        console.log(`[phase1] ${signal}: persisted partial session before exit`);
      } else {
        console.log(`[phase1] ${signal}: nothing to persist (READY not yet received)`);
      }
    } catch (err) {
      console.error(`[phase1] ${signal}: persistSession threw, exiting anyway`, err.message);
      process.exit(1);
    }
    process.exit(signal === 'SIGINT' ? 130 : 143);
  };
  process.on('SIGINT', () => persistAndExit('SIGINT'));
  process.on('SIGTERM', () => persistAndExit('SIGTERM'));

  // Race manager.connect() against a 30s timeout so a Discord-side rate
  // limit doesn't hang the spike forever. .unref() prevents the dangling
  // timer from pinning the event loop past a successful connect.
  // On timeout, destroy the manager before re-throwing so we don't leak
  // an in-flight WS handle into process.exit's lap.
  const CONNECT_TIMEOUT_MS = 30_000;
  try {
    await Promise.race([
      manager.connect(),
      new Promise((_, reject) => {
        const t = setTimeout(
          () => reject(new Error(`connect() timed out after ${CONNECT_TIMEOUT_MS}ms`)),
          CONNECT_TIMEOUT_MS,
        );
        t.unref();
      }),
    ]);
  } catch (err) {
    await manager.destroy({ code: 1000 }).catch(() => { /* shutting down anyway */ });
    throw err;
  }
  console.log('[phase1] connected; idling for', PHASE1_LIFETIME_MS, 'ms to accumulate sequence');
  await new Promise((resolve) => setTimeout(resolve, PHASE1_LIFETIME_MS));

  if (!latestSessionInfo) {
    console.error('[phase1] FAIL: no session info captured. Did READY fire?');
    await manager.destroy({ code: 1000 });
    process.exit(1);
  }

  persistSession(latestSessionInfo);
  console.log('[phase1] persisted session state to', SESSION_FILE);

  // DO NOT call manager.destroy() here. Reading @discordjs/ws's destroy
  // implementation (dist/index.js around line 733): destroy() unconditionally
  // calls updateSessionInfo(shardId, null) unless `recover: Resume` is set,
  // AND sends a WS close-1000 frame. Both signals invalidate the session
  // from Discord's perspective — phase2's RESUME would then hit a deleted
  // session and fall back to IDENTIFY. The library's `recover: Resume`
  // path uses code 4200 (which Discord treats as resumable) but ALSO
  // triggers internalConnect() in the same process, which would resume
  // back into phase1 instead of leaving the session for phase2.
  //
  // The production equivalent is ECS SIGTERMing the task: persist state,
  // then exit. TCP connection drops without a clean close frame; Discord
  // sees a network-level disconnect and preserves the session for the
  // resume buffer window (~60s) — exactly the shape we need for cross-
  // process handoff. The spike emulates this: persist + exit, no clean
  // close.
  console.log('[phase1] exiting without close-frame (TCP-drop emulates ECS SIGTERM);');
  console.log('[phase1] session stays resumable on Discord for ~60s.');
  console.log('[phase1] Run phase2 within ~60s.');
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

  // Mirror of @discordjs/ws's session view: starts as the loaded session,
  // gets cleared to null when the library tells us via updateSessionInfo.
  // CRITICAL: retrieveSessionInfo MUST honor this clear. Returning the
  // loaded session unconditionally produces an infinite RESUME-reject loop —
  // Discord rejects, lib clears, we hand back the (now-dead) session again,
  // Discord rejects again. The spike's first real-world run against the
  // sandbox token surfaced this exact loop. Production DDB-backed code has
  // the same contract: respect the null clear or loop forever.
  let currentSession = stored;
  // Defensive cap: bound IDENTIFY attempts in case of unexpected churn
  // (e.g. another process is contending for the same token). On Fargate
  // these would burn Discord's 1000/24h IDENTIFY budget; same idea here.
  let identifyAttempts = 0;
  // MAX = 1 + guard `> MAX` lets through exactly one IDENTIFY after a
  // RESUME rejection. The earlier guard was `> 2`, which let two
  // through.
  const MAX_IDENTIFY_ATTEMPTS = 1;
  let budgetExhausted = false;

  const manager = new WebSocketManager({
    token,
    intents: INTENTS,
    rest,
    retrieveSessionInfo: (shardId) => {
      if (currentSession) {
        console.log(`[phase2] retrieveSessionInfo(${shardId}) -> stored session (will RESUME)`);
        return currentSession;
      }
      // Short-circuit on prior budget exhaustion so the counter doesn't
      // overrun MAX in subsequent callback invocations and the log line
      // doesn't print "attempt 3/1 / 4/1 / ..." misleadingly. The
      // outer flow already surfaces exit-3 from the budgetExhausted
      // flag; we just want subsequent callbacks to re-throw cleanly.
      if (budgetExhausted) {
        const err = new Error(`IDENTIFY budget exhausted (post-throw callback)`);
        err.code = 'SPIKE_IDENTIFY_BUDGET';
        throw err;
      }
      identifyAttempts += 1;
      if (identifyAttempts > MAX_IDENTIFY_ATTEMPTS) {
        // Budget burned. Setting a flag (alongside the throw) so the
        // outer flow can classify exit-3 cleanly — a thrown error
        // alone would route through connect()'s catch and not reach
        // the dispatch classifier.
        budgetExhausted = true;
        console.log(`[phase2] retrieveSessionInfo(${shardId}) -> null (BUDGET EXHAUSTED after ${MAX_IDENTIFY_ATTEMPTS} attempt(s), aborting)`);
        const err = new Error(`IDENTIFY budget exhausted (${identifyAttempts} attempts)`);
        err.code = 'SPIKE_IDENTIFY_BUDGET';
        throw err;
      }
      console.log(`[phase2] retrieveSessionInfo(${shardId}) -> null (will IDENTIFY, attempt ${identifyAttempts}/${MAX_IDENTIFY_ATTEMPTS})`);
      return null;
    },
    updateSessionInfo: (shardId, info) => {
      if (info === null) {
        // Discord rejected the resume (most commonly: session aged out
        // past the ~60s window, version skew, or another process
        // IDENTIFY'd on the same token and invalidated this session).
        // @discordjs/ws clears session info and will issue a fresh
        // IDENTIFY next — so we clear our mirror too.
        console.log(`[phase2] updateSessionInfo(${shardId}) -> null (RESUME REJECTED — clearing local mirror)`);
        postResumeSessionCleared = true;
        currentSession = null;
      } else {
        console.log(`[phase2] updateSessionInfo(${shardId}) seq=${info.sequence} session=${info.sessionId.slice(0, 8)}…`);
        currentSession = info;
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

  // manager.connect() resolves once the underlying @discordjs/ws layer
  // considers the shard "available." That signal can lag a clean RESUME —
  // the WS is open and dispatching events before connect() resolves.
  // Race it against a global timeout so the spike doesn't hang on
  // unexpected library timing. The Dispatch handler will set resumed /
  // identified regardless of whether connect() has resolved yet.
  const GLOBAL_TIMEOUT_MS = 15_000;
  const connectPromise = manager.connect().catch((err) => {
    // SPIKE_IDENTIFY_BUDGET errors come from our retrieveSessionInfo guard
    // and are an intentional abort — surface them but don't crash the
    // outer flow, so we can still report cleanly.
    console.error(`[phase2] connect() threw: ${err.code || ''} ${err.message}`);
  });
  // .unref() on both timers below so they don't pin the event loop past
  // a successful connect — matches phase1's pattern. Today phase2 always
  // process.exit's so the leak is benign, but if anyone restructures
  // phase2 to not always exit synchronously, .unref() is what keeps the
  // dangling timer from leaking. Phase1 had this; phase2 didn't.
  await Promise.race([
    connectPromise,
    new Promise((resolve) => {
      const t = setTimeout(resolve, GLOBAL_TIMEOUT_MS);
      t.unref();
    }),
  ]);

  // Give the gateway a small window after connect resolves (or after we
  // gave up waiting on it) to deliver the RESUMED/READY dispatch. 3s is
  // more than enough — both events fire within a second on the happy path.
  await new Promise((resolve) => {
    const t = setTimeout(resolve, 3000);
    t.unref();
  });

  await manager.destroy({ code: 1000 }).catch(() => { /* shutting down anyway */ });

  const classification = classifyResult({ resumed, budgetExhausted, identified, postResumeSessionCleared });
  if (classification.severity === 'error') {
    for (const line of classification.lines) console.error(line);
  } else {
    for (const line of classification.lines) console.log(line);
  }
  process.exit(classification.exitCode);
}

/**
 * Pure result-classification over the four observed booleans from phase2.
 * Extracted so unit tests can pin the dispatch order without needing a
 * live Discord connection. Order is load-bearing: budgetExhausted must be
 * checked BEFORE identified-without-budget because READY fires before the
 * second retrieveSessionInfo throws, so the booleans `identified` and
 * `budgetExhausted` can both be true on the same run — and the budget-
 * exhausted classification is the more-specific signal (it tells the
 * operator "token contention is the problem" rather than the generic
 * "RESUME failed but IDENTIFY worked").
 *
 * Exit codes:
 *   0 — RESUME-OK or RESUME-FAIL-with-graceful-IDENTIFY-fallback. Both
 *       indicate the mechanism is working; SLI is just the success rate.
 *   2 — UNCLEAR. Neither RESUMED nor READY observed within timeout —
 *       most often a Discord-side problem or a connect() hang we didn't
 *       race against.
 *   3 — RESUME-FAIL + IDENTIFY-budget-exhausted. Almost always means
 *       token contention; the operator's fix is to scale the contending
 *       process down before re-running.
 *
 * @param {{ resumed: boolean, budgetExhausted: boolean, identified: boolean, postResumeSessionCleared: boolean }} flags
 * @returns {{ exitCode: number, severity: 'log' | 'error', lines: string[] }}
 */
function classifyResult({ resumed, budgetExhausted, identified, postResumeSessionCleared }) {
  if (resumed) {
    return {
      exitCode: 0,
      severity: 'log',
      lines: ['[phase2] RESULT: RESUME-OK (Pillar 2 mechanism validated)'],
    };
  }
  if (budgetExhausted) {
    return {
      exitCode: 3,
      severity: 'error',
      lines: [
        '[phase2] RESULT: RESUME-FAIL + IDENTIFY-budget-exhausted.',
        '[phase2] This usually means token contention — another process is IDENTIFYing',
        '[phase2] on the same token. Check that no other gateway task is running',
        '[phase2] (scale bot_gateway to desired_count=0 before re-running the spike).',
      ],
    };
  }
  if (identified && postResumeSessionCleared) {
    return {
      exitCode: 0,
      severity: 'log',
      lines: [
        '[phase2] RESULT: RESUME-FAIL → IDENTIFY-fallback (graceful)',
        '[phase2] Cause is usually session-aged-out (>60s since phase1), version skew,',
        '[phase2] or another process IDENTIFYing on the same token. The mechanism still',
        '[phase2] works in production — design treats this as a counted SLI (resume',
        '[phase2] success rate) with IDENTIFY as a safety net.',
      ],
    };
  }
  return {
    exitCode: 2,
    severity: 'error',
    lines: ['[phase2] RESULT: UNCLEAR — neither RESUMED nor READY observed within timeout'],
  };
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
  classifyResult,
  // Re-exported so tests can pin the default. Not load-bearing; just
  // saves a require in the test file.
  SESSION_FILE,
};
