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

  test('FLOW_* literals match the snake_case-lower convention used by all other audit events', () => {
    // Convention check: every audit event literal is lower_snake_case
    // (no uppercase, no hyphens, no spaces). CloudWatch metric filter
    // patterns happen to be case-sensitive AND the rest of the
    // codebase's filters at qurl-integrations-infra assume
    // lower_snake_case. A capital letter slipping into a new event
    // name would silently miss every filter.
    for (const key of ['FLOW_CREATED', 'FLOW_TRANSITION', 'FLOW_DELETED']) {
      const literal = AUDIT_EVENTS[key];
      expect(literal).toMatch(/^[a-z][a-z0-9_]*$/);
    }
  });
});
