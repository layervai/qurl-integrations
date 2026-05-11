/**
 * Contract test for the event-shipper Pillar 1 audit events.
 *
 * These three event names (FLOW_CREATED, FLOW_TRANSITION, FLOW_DELETED)
 * are reserved by qurl-integrations-infra's matching CloudWatch metric
 * filters (separate PR, paired with PR 4's emissions). The literal
 * string values MUST stay stable — renaming any of them silently
 * breaks the metric filters with no compile-time signal, because
 * CloudWatch metric filters match log-line JSON against literal
 * strings.
 *
 * Lock the values here so a casual edit to `constants.js` (typo,
 * "let's snake-case this better", AI-suggested rename) fails the
 * test instead of the filter.
 */
const { AUDIT_EVENTS } = require('../src/constants');

describe('AUDIT_EVENTS — event-shipper Pillar 1 (flow_state)', () => {
  test('FLOW_CREATED literal is "flow_created"', () => {
    expect(AUDIT_EVENTS.FLOW_CREATED).toBe('flow_created');
  });

  test('FLOW_TRANSITION literal is "flow_transition"', () => {
    expect(AUDIT_EVENTS.FLOW_TRANSITION).toBe('flow_transition');
  });

  test('FLOW_DELETED literal is "flow_deleted"', () => {
    expect(AUDIT_EVENTS.FLOW_DELETED).toBe('flow_deleted');
  });

  test('AUDIT_EVENTS remains frozen', () => {
    // The freeze is the second line of defense against a runtime
    // mutation breaking metric filters. Pin it here so a future
    // refactor that drops the Object.freeze() call gets caught.
    expect(Object.isFrozen(AUDIT_EVENTS)).toBe(true);
  });

  test('every AUDIT_EVENTS literal follows the lower_snake_case convention', () => {
    // Convention check across the WHOLE object, not just the FLOW_*
    // entries this PR adds — turns this test into a regression guard
    // for every current and future event name. Convention: every
    // audit event literal is lower_snake_case (no uppercase, no
    // hyphens, no spaces). CloudWatch metric filter patterns are
    // case-sensitive AND the rest of the codebase's filters at
    // qurl-integrations-infra assume lower_snake_case. A capital
    // letter slipping into any new event name would silently miss
    // every filter that targets it.
    for (const [key, literal] of Object.entries(AUDIT_EVENTS)) {
      expect(literal).toMatch(/^[a-z][a-z0-9_]*$/);
      // Pin a sane upper bound on the literal length too — CloudWatch
      // metric filter pattern length is bounded, and a runaway literal
      // would silently misbehave at the filter layer. 80 chars is well
      // above every current literal (longest today is
      // `gateway_heartbeat_unhealthy` at 26).
      expect(literal.length).toBeLessThanOrEqual(80);
      // Sanity: key must be UPPER_SNAKE_CASE per file convention.
      // Catches a future contributor adding `flowCreated: 'flow_created'`
      // which would still pass the literal-shape check.
      expect(key).toMatch(/^[A-Z][A-Z0-9_]*$/);
    }
  });
});
