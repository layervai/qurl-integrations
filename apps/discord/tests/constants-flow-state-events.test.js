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
    expect(Object.isFrozen(AUDIT_EVENTS)).toBe(true);
  });

  test('every AUDIT_EVENTS literal follows the lower_snake_case convention', () => {
    for (const [key, literal] of Object.entries(AUDIT_EVENTS)) {
      expect(literal).toMatch(/^[a-z][a-z0-9_]*$/);
      expect(literal.length).toBeLessThanOrEqual(80);
      expect(key).toMatch(/^[A-Z][A-Z0-9_]*$/);
    }
  });
});
