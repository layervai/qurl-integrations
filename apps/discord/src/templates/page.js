// HTML page template for OAuth responses
const { COLORS } = require('../constants');

// Convert hex int to CSS color
function hexToColor(hex) {
  return '#' + hex.toString(16).padStart(6, '0');
}

function escapeHtml(str) {
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

const TYPE_COLORS = {
  success: { bg: hexToColor(COLORS.SUCCESS), icon: hexToColor(COLORS.SUCCESS) },
  error: { bg: hexToColor(COLORS.ERROR), icon: hexToColor(COLORS.ERROR) },
  warning: { bg: hexToColor(COLORS.WARNING), icon: hexToColor(COLORS.WARNING) },
  info: { bg: hexToColor(COLORS.PRIMARY), icon: hexToColor(COLORS.PRIMARY) },
};

/**
 * Render a styled HTML page
 * @param {Object} options
 * @param {string} options.title - Page title
 * @param {string} options.icon - Emoji icon
 * @param {string} options.heading - Main heading
 * @param {string} options.message - Message body (HTML-escaped at render time)
 * @param {string} [options.subtext] - Optional subtext
 * @param {'success'|'error'|'warning'|'info'} [options.type='info'] - Page type for coloring
 * @param {boolean} [options.showDiscordButton=false] - Show "Open Discord" button
 */
function renderPage({ title, icon, heading, message, subtext, type = 'info', showDiscordButton = false }) {
  const color = TYPE_COLORS[type] || TYPE_COLORS.info;

  return `
    <!DOCTYPE html>
    <html>
    <head>
      <meta charset="utf-8">
      <meta name="viewport" content="width=device-width, initial-scale=1">
      <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:">
      <title>${escapeHtml(title)} - QURL</title>
      <style>
        * { box-sizing: border-box; }
        body {
          font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
          display: flex;
          justify-content: center;
          align-items: center;
          min-height: 100vh;
          margin: 0;
          padding: 20px;
          background: linear-gradient(135deg, #1a1a2e 0%, #16213e 100%);
          color: white;
        }
        .container {
          text-align: center;
          padding: 40px;
          background: rgba(255,255,255,0.1);
          border-radius: 16px;
          backdrop-filter: blur(10px);
          max-width: 480px;
          width: 100%;
          box-shadow: 0 8px 32px rgba(0,0,0,0.3);
        }
        .icon {
          font-size: 64px;
          margin-bottom: 16px;
        }
        h1 {
          color: ${color.icon};
          margin: 0 0 16px 0;
          font-size: 24px;
        }
        .message {
          color: #e0e0e0;
          margin: 0 0 12px 0;
          font-size: 16px;
          line-height: 1.5;
        }
        .subtext {
          font-size: 14px;
          color: #888;
          margin-top: 20px;
        }
        .username {
          color: #3498DB;
          font-weight: bold;
        }
        .btn {
          display: inline-block;
          margin-top: 24px;
          padding: 12px 24px;
          background: #5865F2;
          color: white;
          text-decoration: none;
          border-radius: 8px;
          font-weight: 600;
          transition: background 0.2s;
        }
        .btn:hover {
          background: #4752C4;
        }
        .btn-secondary {
          background: transparent;
          border: 1px solid #555;
          color: #aaa;
          margin-left: 12px;
        }
        .btn-secondary:hover {
          background: rgba(255,255,255,0.1);
          border-color: #777;
        }
      </style>
    </head>
    <body>
      <div class="container">
        <div class="icon">${escapeHtml(icon)}</div>
        <h1>${escapeHtml(heading)}</h1>
        <p class="message">${escapeHtml(message)}</p>
        ${subtext ? `<p class="subtext">${escapeHtml(subtext)}</p>` : ''}
        ${showDiscordButton ? `
          <a href="discord://" class="btn">Open Discord</a>
        ` : ''}
      </div>
    </body>
    </html>
  `;
}

module.exports = { renderPage, escapeHtml };
