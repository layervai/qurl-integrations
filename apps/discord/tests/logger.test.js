/**
 * Tests for src/logger.js
 */

describe('logger', () => {
  let logger;
  let originalLogLevel;
  let consoleSpy;

  beforeEach(() => {
    originalLogLevel = process.env.LOG_LEVEL;
    // Reset modules to pick up env changes
    jest.resetModules();
    consoleSpy = {
      error: jest.spyOn(console, 'error').mockImplementation(),
      warn: jest.spyOn(console, 'warn').mockImplementation(),
      log: jest.spyOn(console, 'log').mockImplementation(),
    };
  });

  afterEach(() => {
    if (originalLogLevel !== undefined) {
      process.env.LOG_LEVEL = originalLogLevel;
    } else {
      delete process.env.LOG_LEVEL;
    }
    jest.restoreAllMocks();
  });

  it('info level logs info, warn, error but not debug', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.error('err msg', { key: 'val' });
    logger.warn('warn msg');
    logger.info('info msg');
    logger.debug('debug msg');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.error.mock.calls[0][0]).toContain('ERROR: err msg');
    expect(consoleSpy.warn).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn.mock.calls[0][0]).toContain('WARN: warn msg');
    expect(consoleSpy.log).toHaveBeenCalledTimes(1); // only info, not debug
    expect(consoleSpy.log.mock.calls[0][0]).toContain('INFO: info msg');
  });

  it('debug level logs everything', () => {
    process.env.LOG_LEVEL = 'debug';
    logger = require('../src/logger');

    logger.error('e');
    logger.warn('w');
    logger.info('i');
    logger.debug('d');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn).toHaveBeenCalledTimes(1);
    expect(consoleSpy.log).toHaveBeenCalledTimes(2); // info + debug
  });

  it('error level only logs errors', () => {
    process.env.LOG_LEVEL = 'error';
    logger = require('../src/logger');

    logger.error('e');
    logger.warn('w');
    logger.info('i');
    logger.debug('d');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn).not.toHaveBeenCalled();
    expect(consoleSpy.log).not.toHaveBeenCalled();
  });

  it('warn level logs warn and error', () => {
    process.env.LOG_LEVEL = 'warn';
    logger = require('../src/logger');

    logger.error('e');
    logger.warn('w');
    logger.info('i');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn).toHaveBeenCalledTimes(1);
    expect(consoleSpy.log).not.toHaveBeenCalled();
  });

  it('defaults to info level when LOG_LEVEL is not set', () => {
    delete process.env.LOG_LEVEL;
    logger = require('../src/logger');

    logger.info('i');
    logger.debug('d');

    expect(consoleSpy.log).toHaveBeenCalledTimes(1);
  });

  it('includes meta as JSON when provided', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('test', { foo: 'bar' });

    expect(consoleSpy.log.mock.calls[0][0]).toContain('{"foo":"bar"}');
  });

  it('omits meta string when no meta keys', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('test');

    const output = consoleSpy.log.mock.calls[0][0];
    expect(output).toContain('INFO: test');
    // Should not have trailing JSON
    expect(output).not.toContain('{}');
  });

  it('includes ISO timestamp in output', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('ts-test');

    const output = consoleSpy.log.mock.calls[0][0];
    // ISO format: [2026-...T...Z]
    expect(output).toMatch(/\[\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
  });
});
