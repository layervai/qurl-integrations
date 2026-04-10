// Simple structured logger with timestamps

const levels = {
  error: 0,
  warn: 1,
  info: 2,
  debug: 3,
};

const currentLevel = levels[process.env.LOG_LEVEL] ?? levels.info;

function formatTimestamp() {
  return new Date().toISOString();
}

function formatMessage(level, message, meta = {}) {
  const metaStr = Object.keys(meta).length > 0 ? ` ${JSON.stringify(meta)}` : '';
  return `[${formatTimestamp()}] ${level.toUpperCase()}: ${message}${metaStr}`;
}

const logger = {
  error(message, meta = {}) {
    if (currentLevel >= levels.error) {
      console.error(formatMessage('error', message, meta));
    }
  },

  warn(message, meta = {}) {
    if (currentLevel >= levels.warn) {
      console.warn(formatMessage('warn', message, meta));
    }
  },

  info(message, meta = {}) {
    if (currentLevel >= levels.info) {
      console.log(formatMessage('info', message, meta));
    }
  },

  debug(message, meta = {}) {
    if (currentLevel >= levels.debug) {
      console.log(formatMessage('debug', message, meta));
    }
  },
};

module.exports = logger;
