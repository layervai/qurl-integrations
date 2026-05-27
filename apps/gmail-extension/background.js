/**
 * qURL Gmail Extension — Service Worker (Background Script)
 *
 * MV3 service worker: relays popup messages to the active Gmail tab
 * and reinjects content scripts when Gmail loses them after navigation.
 */

'use strict';

if (typeof importScripts === 'function') {
  importScripts('lib/qurl-i18n.js');
}

const TAB_MESSAGE_TIMEOUT_MS = 3000;
// The full relay path from popup to content script is:
// ping (TAB_MESSAGE_TIMEOUT_MS) -> reinject -> ping (TAB_MESSAGE_TIMEOUT_MS) -> INSERT_LINKS relay.
// popup/popup.js:RUNTIME_MESSAGE_TIMEOUT_MS must exceed this worst-case budget.
// Keep these values in sync — see the matching comment in popup.js.
const INSERT_LINKS_TAB_MESSAGE_TIMEOUT_MS = 9000;

function isGmailTab(tab) {
  return Boolean(tab && tab.id && typeof tab.url === 'string' && tab.url.startsWith('https://mail.google.com/mail/'));
}

function getMessage(key, fallback, substitutions) {
  if (typeof QURLI18n !== 'undefined' && typeof QURLI18n.getMessage === 'function') {
    return QURLI18n.getMessage(key, fallback, substitutions);
  }
  return fallback || '';
}

function isTrustedInsertLinksSender(sender) {
  if (typeof chrome === 'undefined' || !chrome.runtime || !chrome.runtime.id) {
    return false;
  }

  // Extension pages such as the popup have no sender.tab; content scripts and web-page contexts do.
  return Boolean(sender && sender.id === chrome.runtime.id && !sender.tab);
}

function formatErrorMessage(err, fallback) {
  if (err && typeof err.message === 'string' && err.message) {
    return err.message;
  }
  if (err !== undefined && err !== null && String(err)) {
    return String(err);
  }
  return fallback || '';
}

function sendMessageToTab(tabId, message, timeoutMs) {
  return new Promise(function (resolve, reject) {
    let settled = false;
    const timerId = setTimeout(function () {
      if (settled) {
        return;
      }
      settled = true;
      reject(new Error(getMessage(
        'gmail_response_timeout',
        'Timed out while waiting for the Gmail tab to respond.'
      )));
    }, timeoutMs || TAB_MESSAGE_TIMEOUT_MS);

    chrome.tabs.sendMessage(tabId, message, function (resp) {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timerId);
      if (chrome.runtime.lastError) {
        reject(new Error(chrome.runtime.lastError.message));
        return;
      }
      resolve(resp);
    });
  });
}

async function ensureGmailContentScript(tab) {
  if (!isGmailTab(tab)) {
    throw new Error(getMessage(
      'active_tab_not_gmail_error',
      'Active tab is not Gmail. Please switch to an open Gmail compose tab.'
    ));
  }

  try {
    await sendMessageToTab(tab.id, { type: 'QURL_PING' });
    return;
  } catch (_err) {
    // Reinject on any ping failure. The content script has a double-injection guard,
    // so this avoids brittle Chrome error-string matching.
  }

  await chrome.scripting.executeScript({
    target: { tabId: tab.id },
    files: ['lib/qurl-i18n.js', 'lib/qurl-compose-format.js', 'content/gmail-compose.js'],
  });

  await sendMessageToTab(tab.id, { type: 'QURL_PING' });
}

async function relayInsertLinks(message) {
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });

  if (!tabs || tabs.length === 0 || !tabs[0].id) {
    return {
      success: false,
      error: getMessage('no_active_tab_error', 'No active tab found'),
    };
  }

  const activeTab = tabs[0];
  await ensureGmailContentScript(activeTab);
  const response = await sendMessageToTab(activeTab.id, message, INSERT_LINKS_TAB_MESSAGE_TIMEOUT_MS);
  return response || {
    success: false,
    error: getMessage('no_response_from_content_script', 'No response from content script'),
  };
}

if (typeof chrome !== 'undefined' && chrome.runtime && chrome.runtime.onMessage) {
  chrome.runtime.onMessage.addListener(function (message, sender, sendResponse) {
    // Relay INSERT_LINKS from popup to the Gmail content script in the active tab.
    // MV3 blocks direct popup → content script communication.
    // Only extension UI pages should be able to trigger insertion through this bridge.
    if (message.type !== 'INSERT_LINKS') {
      return false;
    }
    if (!isTrustedInsertLinksSender(sender)) {
      return false;
    }

    relayInsertLinks(message)
      .then(function (response) {
        sendResponse(response);
      })
      .catch(function (err) {
        sendResponse({
          success: false,
          error: formatErrorMessage(err, getMessage('gmail_insert_failed', 'Failed to insert links into the Gmail draft.')),
        });
      });
    return true; // async response expected
  });
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    ensureGmailContentScript,
    getMessage,
    formatErrorMessage,
    isTrustedInsertLinksSender,
    isGmailTab,
    INSERT_LINKS_TAB_MESSAGE_TIMEOUT_MS,
    relayInsertLinks,
    TAB_MESSAGE_TIMEOUT_MS,
    sendMessageToTab,
  };
}
