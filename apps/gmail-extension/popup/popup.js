/**
 * qURL Gmail Extension — Popup Logic
 *
 * Handles file selection, upload orchestration, and result reporting.
 */

'use strict';

// ==================== DOM References ====================
const fileInput = document.getElementById('fileInput');
const selectBtn = document.getElementById('selectBtn');
const fileCount = document.getElementById('fileCount');
const fileList = document.getElementById('fileList');
const uploadBtn = document.getElementById('uploadBtn');
const progressArea = document.getElementById('progressArea');
const resultArea = document.getElementById('resultArea');
const errorArea = document.getElementById('errorArea');
const settingsBtn = document.getElementById('settingsBtn');
const settingsPanel = document.getElementById('settingsPanel');
const settingsCloseBtn = document.getElementById('settingsCloseBtn');
const apiBaseInput = document.getElementById('apiBaseInput');
const saveConfigBtn = document.getElementById('saveConfigBtn');
const resetConfigBtn = document.getElementById('resetConfigBtn');
const permissionConfirmPanel = document.getElementById('permissionConfirmPanel');
const permissionConfirmText = document.getElementById('permissionConfirmText');
const permissionConfirmContinueBtn = document.getElementById('permissionConfirmContinueBtn');
const permissionConfirmCancelBtn = document.getElementById('permissionConfirmCancelBtn');
const configHint = document.getElementById('configHint');
const copyArea = document.getElementById('copyArea');
const copyBtn = document.getElementById('copyBtn');
const footer = document.querySelector('.footer');
// Keep the popup budget above the full relay path:
// ping (3s) -> reinject -> ping (3s) -> INSERT_LINKS relay (9s).
const RUNTIME_MESSAGE_TIMEOUT_MS = 16000;
const RUNTIME_MESSAGE_RETRY_DELAY_MS = 250;
const SETTINGS_PANEL_AUTO_CLOSE_MS = 1200;
const COPY_BUTTON_REVERT_MS = 1500;
const FOCUS_DEFER_MS = 0;

// ==================== State ====================
let selectedFiles = [];
let lastSuccessfulResults = [];
let settingsPanelCloseTimer = null;
let pendingPermissionRequest = null;

applyLocalizedText();
initializePopup();

// ==================== File Selection ====================

selectBtn.addEventListener('click', () => {
  fileInput.click();
});

fileInput.addEventListener('change', () => {
  selectedFiles = Array.from(fileInput.files);
  renderFileList();
  updateUploadButton();
});

settingsBtn.addEventListener('click', (event) => {
  event.stopPropagation();
  if (isSettingsPanelOpen()) {
    closeSettingsPanel();
    return;
  }
  openSettingsPanel();
});

settingsCloseBtn.addEventListener('click', () => {
  closeSettingsPanel();
});

settingsPanel.addEventListener('click', (event) => {
  event.stopPropagation();
});

apiBaseInput.addEventListener('focus', () => {
  clearSettingsPanelCloseTimer();
});

apiBaseInput.addEventListener('input', () => {
  clearSettingsPanelCloseTimer();
  if (pendingPermissionRequest && apiBaseInput.value !== pendingPermissionRequest.originalValue) {
    hidePermissionConfirmation();
  }
});

document.addEventListener('click', (event) => {
  if (!isSettingsPanelOpen()) return;

  const target = event.target;
  if (target && typeof target.closest === 'function' && target.closest('#settingsPanel, #settingsBtn')) {
    return;
  }

  closeSettingsPanel();
});

document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && isSettingsPanelOpen()) {
    closeSettingsPanel();
  }
});

saveConfigBtn.addEventListener('click', async () => {
  const value = apiBaseInput.value;
  let confirmation = null;
  try {
    confirmation = getCustomServerPermissionConfirmation(value);
  } catch (err) {
    hidePermissionConfirmation();
    setConfigHint(err.message || getMessage('config_save_error', 'Failed to save qURL server URL.'), 'error');
    return;
  }
  if (confirmation) {
    showPermissionConfirmation(confirmation);
    return;
  }

  await persistApiBaseValue(value);
});

resetConfigBtn.addEventListener('click', async () => {
  setConfigButtonsLoading(true);
  try {
    await setStoredQurlApiBase('');
    apiBaseInput.value = '';
    setConfigHint(getMessage('config_default_hint', 'Leave this blank to use the built-in default server.'), 'success');
    setCustomConfigIndicator(false);
    // Keep the panel open after reset so users can immediately enter a new server URL.
  } catch (err) {
    setConfigHint(err.message || getMessage('config_reset_error', 'Failed to reset qURL server URL.'), 'error');
  } finally {
    setConfigButtonsLoading(false);
  }
});

permissionConfirmContinueBtn.addEventListener('click', async () => {
  if (!pendingPermissionRequest) {
    return;
  }

  const request = pendingPermissionRequest;
  hidePermissionConfirmation();

  setConfigButtonsLoading(true);
  try {
    if (typeof requestQurlHostPermission === 'function') {
      const granted = await requestQurlHostPermission(request.normalized);
      if (!granted) {
        setConfigHint(getMessage(
          'permission_request_denied_error',
          'Permission to access this qURL server was not granted.'
        ), 'error');
        return;
      }
    }
    await persistApiBaseValue(request.originalValue, {
      preserveLoadingState: true,
      skipPermissionRequest: true,
    });
  } catch (err) {
    setConfigHint(err.message || getMessage('config_save_error', 'Failed to save qURL server URL.'), 'error');
  } finally {
    setConfigButtonsLoading(false);
  }
});

permissionConfirmCancelBtn.addEventListener('click', () => {
  hidePermissionConfirmation();
  setConfigHint(getMessage(
    'permission_request_cancelled',
    'Custom server access request was canceled.'
  ), '');
});

function renderFileList() {
  fileList.innerHTML = '';
  if (selectedFiles.length === 0) {
    fileCount.textContent = getMessage('no_file_selected', 'No file selected');
    return;
  }

  fileCount.textContent = getMessage('file_count', 'Selected: $1', [String(selectedFiles.length)]);

  selectedFiles.forEach((file, index) => {
    const item = document.createElement('div');
    item.className = 'file-item';
    const fileName = document.createElement('span');
    fileName.className = 'file-name';
    fileName.textContent = file.name;
    fileName.setAttribute('title', file.name);

    const fileSize = document.createElement('span');
    fileSize.className = 'file-size';
    fileSize.textContent = formatFileSize(file.size);

    const removeButton = document.createElement('button');
    const removeLabel = getMessage('file_remove_label', 'Remove file');
    removeButton.className = 'file-remove';
    removeButton.dataset.index = String(index);
    removeButton.setAttribute('title', removeLabel);
    removeButton.setAttribute('aria-label', removeLabel);
    removeButton.innerHTML = `
      <svg width="10" height="10" viewBox="0 0 10 10" fill="none">
        <path d="M1 1L9 9M9 1L1 9" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
      </svg>
    `;

    item.appendChild(fileName);
    item.appendChild(fileSize);
    item.appendChild(removeButton);
    fileList.appendChild(item);
  });

  // Remove button handlers
  fileList.querySelectorAll('.file-remove').forEach(btn => {
    btn.addEventListener('click', (e) => {
      const idx = parseInt(e.currentTarget.dataset.index, 10);
      // Guard against missing or malformed data-index attribute
      if (Number.isNaN(idx) || idx < 0 || idx >= selectedFiles.length) {
        return;
      }
      removeFile(idx);
    });
  });
}

function removeFile(index) {
  const dt = new DataTransfer();
  selectedFiles.splice(index, 1);
  selectedFiles.forEach(f => dt.items.add(f));
  fileInput.files = dt.files;
  renderFileList();
  updateUploadButton();
}

function updateUploadButton() {
  uploadBtn.disabled = selectedFiles.length === 0;
}

async function initializePopup() {
  try {
    const stored = await getStoredQurlApiBase();
    if (stored) {
      apiBaseInput.value = stored;
      setConfigHint(getMessage('config_custom_active', 'Custom server is active.'), '');
      setCustomConfigIndicator(true);
    } else {
      apiBaseInput.value = '';
      setConfigHint(getMessage('config_default_hint', 'Leave this blank to use the built-in default server.'), '');
      setCustomConfigIndicator(false);
    }
  } catch (err) {
    setConfigHint(err.message || getMessage('config_load_error', 'Failed to load qURL server configuration.'), 'error');
  }
}

function setConfigButtonsLoading(loading) {
  saveConfigBtn.disabled = loading;
  resetConfigBtn.disabled = loading;
  apiBaseInput.disabled = loading;
  permissionConfirmContinueBtn.disabled = loading;
  permissionConfirmCancelBtn.disabled = loading;
}

function setConfigHint(message, state) {
  configHint.textContent = message;
  configHint.classList.remove('success', 'error');
  if (state) {
    configHint.classList.add(state);
  }
}

function setCustomConfigIndicator(enabled) {
  settingsBtn.classList.toggle('has-custom-config', enabled);
}

function isSettingsPanelOpen() {
  return !settingsPanel.classList.contains('hidden');
}

function openSettingsPanel() {
  clearSettingsPanelCloseTimer();
  settingsPanel.classList.remove('hidden');
  settingsBtn.setAttribute('aria-expanded', 'true');
  hidePermissionConfirmation();
  window.setTimeout(function () {
    apiBaseInput.focus();
    apiBaseInput.select();
  }, FOCUS_DEFER_MS);
}

function closeSettingsPanel() {
  clearSettingsPanelCloseTimer();
  if (!isSettingsPanelOpen()) return;
  hidePermissionConfirmation();
  settingsPanel.classList.add('hidden');
  settingsBtn.setAttribute('aria-expanded', 'false');
}

function scheduleSettingsPanelClose() {
  clearSettingsPanelCloseTimer();
  settingsPanelCloseTimer = window.setTimeout(function () {
    closeSettingsPanel();
  }, SETTINGS_PANEL_AUTO_CLOSE_MS);
}

function clearSettingsPanelCloseTimer() {
  if (settingsPanelCloseTimer !== null) {
    window.clearTimeout(settingsPanelCloseTimer);
    settingsPanelCloseTimer = null;
  }
}

// ==================== Upload ====================

uploadBtn.addEventListener('click', async () => {
  if (selectedFiles.length === 0) return;

  // Reset UI
  clearResults();
  setLoading(true);
  footer.textContent = getMessage('uploading_hint', 'Uploading, please wait...');

  const results = [];
  const errors = [];
  let insertionError = null;

  for (const file of selectedFiles) {
    // Add progress item
    const progressItem = addProgressItem(file.name, 'uploading');

    try {
      const buf = await file.arrayBuffer();
      const result = await uploadFile(buf, file.name, file.type);

      if (result.success) {
        results.push({
          filename: file.name,
          link: result.qurl_link || result.resource_url || '',
          expiry: result.expires_at || null,
        });
        updateProgressItem(progressItem, 'success', file.name);
      } else {
        errors.push({ filename: file.name, error: result.error });
        updateProgressItem(progressItem, 'error', `${file.name}: ${result.error}`);
      }
    } catch (err) {
      errors.push({ filename: file.name, error: err.message });
      updateProgressItem(progressItem, 'error', `${file.name}: ${err.message}`);
    }
  }

  // Insert into Gmail compose
  if (results.length > 0) {
    footer.textContent = getMessage('inserting_hint', 'Upload complete. Inserting links into Gmail...');
    insertionError = await insertIntoGmailDraft(results);
  }

  lastSuccessfulResults = results.slice();

  // Show results
  showResults(results, errors, insertionError);
  setLoading(false);
  footer.textContent = getMessage('footer_hint', 'Make sure a Gmail compose window is open.');
});

copyBtn.addEventListener('click', async () => {
  if (lastSuccessfulResults.length === 0) return;

  try {
    const html = buildCopyHtml(lastSuccessfulResults);
    const text = buildCopyText(lastSuccessfulResults);
    await writeRichClipboard(html, text);
    copyBtn.textContent = getMessage('copy_done', 'Copied');
  } catch (err) {
    console.warn('[qURL] Copy failed:', err.message);
    copyBtn.textContent = getMessage('copy_failed', 'Copy failed');
  }

  window.setTimeout(function () {
    copyBtn.textContent = getMessage('copy_btn', 'Copy inserted content');
  }, COPY_BUTTON_REVERT_MS);
});

// ==================== Progress UI ====================

function addProgressItem(filename, state) {
  progressArea.classList.remove('hidden');
  const item = document.createElement('div');
  item.className = `progress-item ${state}`;
  item.innerHTML = `
    <div class="progress-icon">${state === 'uploading' ? '<div class="spinner"></div>' : ''}</div>
    <span class="progress-text">${escapeHtml(filename)}</span>
  `;
  progressArea.appendChild(item);
  return item;
}

function updateProgressItem(item, state, text) {
  item.className = `progress-item ${state}`;
  const icon = state === 'success'
    ? '<svg width="14" height="14" viewBox="0 0 14 14" fill="none"><path d="M2 7L5.5 10.5L12 3" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>'
    : state === 'error'
    ? '<svg width="14" height="14" viewBox="0 0 14 14" fill="none"><path d="M3 3L11 11M11 3L3 11" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>'
    : '';
  item.innerHTML = `
    <div class="progress-icon">${icon || '<div class="spinner"></div>'}</div>
    <span class="progress-text">${escapeHtml(text)}</span>
  `;
}

function formatExpiry(isoString) {
  return window.QURLComposeFormatter
    ? window.QURLComposeFormatter.formatExpiry(isoString)
    : null;
}

// ==================== Gmail Draft Insertion ====================

async function insertIntoGmailDraft(results) {
  try {
    const response = await sendRuntimeMessageWithRetry(createInsertLinksMessage(results));
    if (!response || !response.success) {
      return (response && response.error)
        || getMessage('gmail_insert_failed', 'Failed to insert links into the Gmail draft.');
    }
    return null;
  } catch (err) {
    console.warn('[qURL] Could not insert links into Gmail draft:', err.message);
    return err.message
      || getMessage('gmail_insert_failed', 'Failed to insert links into the Gmail draft.');
  }
}

// ==================== Results UI ====================

function showResults(results, errors, insertionError) {
  resultArea.innerHTML = '';
  errorArea.innerHTML = '';
  resultArea.classList.add('hidden');
  errorArea.classList.add('hidden');
  copyArea.classList.add('hidden');
  copyBtn.disabled = true;
  copyBtn.textContent = getMessage('copy_btn', 'Copy inserted content');

  if (results.length === 0 && errors.length === 0 && !insertionError) return;

  if (results.length > 0) {
    copyArea.classList.remove('hidden');
    copyBtn.disabled = false;
    resultArea.classList.remove('hidden');
    const summaryClass = errors.length === 0 ? 'all-success' : 'partial';
    const summaryText = results.length === 1
      ? getMessage('result_one_success', '1 file uploaded successfully')
      : getMessage('result_n_success', '$1 files uploaded successfully', [String(results.length)]);

    const summary = document.createElement('div');
    summary.className = `result-summary ${summaryClass}`;
    summary.textContent = summaryText;
    resultArea.appendChild(summary);

    results.forEach(r => {
      const row = document.createElement('div');
      row.className = 'result-row success';
      const safeHref = normalizeAllowedLink(r.link);
      const label = document.createElement(safeHref ? 'a' : 'span');
      label.className = 'result-link';
      label.textContent = r.filename || getMessage('unnamed_file', 'Unnamed file');
      if (safeHref) {
        label.href = safeHref;
        label.target = '_blank';
        label.rel = 'noopener noreferrer';
      }
      row.appendChild(label);

      const formattedExpiry = formatExpiry(r.expiry);
      if (formattedExpiry) {
        const expiry = document.createElement('span');
        expiry.className = 'result-expiry';
        expiry.textContent = getMessage('expiry_suffix', ' (Expires: $1)', [formattedExpiry]);
        row.appendChild(expiry);
      }

      resultArea.appendChild(row);
    });
  }

  if (errors.length > 0 || insertionError) {
    errorArea.classList.remove('hidden');
    const title = document.createElement('div');
    title.className = 'error-title';
    title.textContent = errors.length === 1 && !insertionError
      ? getMessage('result_one_error', '1 file failed to upload')
      : insertionError && errors.length === 0
      ? getMessage('result_insertion_only_failed', 'Uploaded successfully, but Gmail draft insertion failed')
      : getMessage('result_n_errors', '$1 files failed to upload', [String(errors.length)]);
    errorArea.appendChild(title);

    errors.forEach(e => {
      const msg = document.createElement('div');
      msg.className = 'error-msg';
      msg.textContent = `• ${e.filename}: ${e.error}`;
      errorArea.appendChild(msg);
    });

    if (insertionError) {
      const msg = document.createElement('div');
      msg.className = 'error-msg';
      msg.textContent = `• ${insertionError}`;
      errorArea.appendChild(msg);
    }
  }
}

function clearResults() {
  progressArea.innerHTML = '';
  progressArea.classList.add('hidden');
  resultArea.innerHTML = '';
  resultArea.classList.add('hidden');
  errorArea.innerHTML = '';
  errorArea.classList.add('hidden');
  copyArea.classList.add('hidden');
  copyBtn.disabled = true;
  copyBtn.textContent = getMessage('copy_btn', 'Copy inserted content');
}

function setLoading(loading) {
  uploadBtn.disabled = loading || selectedFiles.length === 0;
  selectBtn.disabled = loading;
  uploadBtn.textContent = loading
    ? getMessage('uploading_label', 'Uploading...')
    : getMessage('upload_btn', 'Upload to qURL');
}

// ==================== Utilities ====================

function getMessage(key, fallback, substitutions) {
  if (typeof QURLI18n !== 'undefined' && typeof QURLI18n.getMessage === 'function') {
    return QURLI18n.getMessage(key, fallback, substitutions);
  }
  return fallback || '';
}

function applyLocalizedText() {
  document.title = getMessage('ext_name', document.title);

  document.querySelectorAll('[data-i18n]').forEach(function (element) {
    const key = element.dataset.i18n;
    const message = getMessage(key, '');
    if (message) {
      element.textContent = message;
    }
  });

  document.querySelectorAll('[data-i18n-attr]').forEach(function (element) {
    const key = element.dataset.i18nAttrKey || element.dataset.i18n;
    const message = getMessage(key, '');
    if (!message) {
      return;
    }

    element.dataset.i18nAttr.split(',').forEach(function (attrName) {
      const trimmed = attrName.trim();
      if (trimmed) {
        element.setAttribute(trimmed, message);
      }
    });
  });
}

async function sendRuntimeMessageWithRetry(message, attempts) {
  const maxAttempts = attempts || 2;
  let lastError;

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      return await sendRuntimeMessageWithTimeout(message, RUNTIME_MESSAGE_TIMEOUT_MS);
    } catch (err) {
      lastError = err;
      if (attempt === maxAttempts || !isRetryableRuntimeMessageError(err)) {
        break;
      }
      await new Promise(function (resolve) {
        window.setTimeout(resolve, RUNTIME_MESSAGE_RETRY_DELAY_MS);
      });
    }
  }

  throw lastError || new Error(getMessage('runtime_message_failed', 'Failed to send runtime message.'));
}

function sendRuntimeMessageWithTimeout(message, timeoutMs) {
  return new Promise(function (resolve, reject) {
    const timerId = window.setTimeout(function () {
      const timeoutError = new Error(getMessage(
        'gmail_response_timeout',
        'Timed out while waiting for the Gmail tab to respond.'
      ));
      timeoutError.qurlRetryable = false;
      timeoutError.qurlErrorCode = 'timeout';
      reject(timeoutError);
    }, timeoutMs);

    chrome.runtime.sendMessage(message).then(function (response) {
      window.clearTimeout(timerId);
      resolve(response);
    }).catch(function (err) {
      window.clearTimeout(timerId);
      const runtimeError = err instanceof Error
        ? err
        : new Error(String(err || getMessage('runtime_message_failed', 'Failed to send runtime message.')));
      runtimeError.qurlRetryable = true;
      runtimeError.qurlErrorCode = 'runtime_send_failed';
      reject(runtimeError);
    });
  });
}

function getCustomServerPermissionConfirmation(value) {
  if (!value || typeof normalizeQurlApiBase !== 'function') {
    return null;
  }

  const normalized = normalizeQurlApiBase(value);
  if (!normalized || typeof isDefaultQurlOrigin !== 'function' || isDefaultQurlOrigin(normalized)) {
    return null;
  }

  return {
    normalized,
    origin: new URL(normalized).origin,
    originalValue: value,
  };
}

function showPermissionConfirmation(details) {
  pendingPermissionRequest = details;
  permissionConfirmText.textContent = getMessage(
    'permission_request_confirm',
    'Allow the extension to access $1 for qURL uploads? Chrome will show a permission prompt next.',
    [details.origin]
  );
  permissionConfirmPanel.classList.remove('hidden');
}

function hidePermissionConfirmation() {
  pendingPermissionRequest = null;
  permissionConfirmPanel.classList.add('hidden');
  permissionConfirmText.textContent = '';
}

async function persistApiBaseValue(value, options) {
  const resolvedOptions = options || {};
  if (!resolvedOptions.preserveLoadingState) {
    setConfigButtonsLoading(true);
  }
  try {
    const saved = await setStoredQurlApiBase(value, {
      skipPermissionRequest: Boolean(resolvedOptions.skipPermissionRequest),
    });
    if (saved) {
      apiBaseInput.value = saved;
      setConfigHint(getMessage('config_custom_saved', 'Custom server saved.'), 'success');
      setCustomConfigIndicator(true);
    } else {
      apiBaseInput.value = '';
      setConfigHint(getMessage('config_default_hint', 'Leave this blank to use the built-in default server.'), 'success');
      setCustomConfigIndicator(false);
    }
    hidePermissionConfirmation();
    scheduleSettingsPanelClose();
  } catch (err) {
    setConfigHint(err.message || getMessage('config_save_error', 'Failed to save qURL server URL.'), 'error');
  } finally {
    if (!resolvedOptions.preserveLoadingState) {
      setConfigButtonsLoading(false);
    }
  }
}

function createInsertLinksMessage(results) {
  return {
    type: 'INSERT_LINKS',
    requestId: createRequestId(),
    results,
  };
}

function createRequestId() {
  if (window.crypto && typeof window.crypto.randomUUID === 'function') {
    return window.crypto.randomUUID();
  }
  return `qurl-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function isRetryableRuntimeMessageError(err) {
  return Boolean(err && err.qurlRetryable);
}

function escapeHtml(str) {
  if (window.QURLComposeFormatter && typeof window.QURLComposeFormatter.escapeHtml === 'function') {
    return window.QURLComposeFormatter.escapeHtml(str);
  }
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function formatFileSize(bytes) {
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  return (bytes / (1024 * 1024 * 1024)).toFixed(1) + ' GB';
}

function buildCopyHtml(results) {
  if (window.QURLComposeFormatter) {
    return window.QURLComposeFormatter.buildLinkHtml(results);
  }
  return '';
}

function buildCopyText(results) {
  if (window.QURLComposeFormatter) {
    return window.QURLComposeFormatter.buildLinkPlainText(results);
  }
  return '';
}

function normalizeAllowedLink(link) {
  if (window.QURLComposeFormatter && typeof window.QURLComposeFormatter.normalizeAllowedLink === 'function') {
    return window.QURLComposeFormatter.normalizeAllowedLink(link);
  }

  if (!link) {
    return null;
  }

  try {
    const parsed = new URL(String(link));
    return parsed.protocol === 'https:' ? parsed.toString() : null;
  } catch (_err) {
    return null;
  }
}

async function writeRichClipboard(html, text) {
  if (navigator.clipboard && window.ClipboardItem) {
    try {
      const item = new ClipboardItem({
        'text/html': new Blob([html], { type: 'text/html' }),
        'text/plain': new Blob([text], { type: 'text/plain' }),
      });
      await navigator.clipboard.write([item]);
      return;
    } catch (err) {
      console.warn('[qURL] navigator.clipboard.write failed, falling back:', err.message);
    }
  }

  copyViaExecCommand(html, text);
}

function copyViaExecCommand(html, text) {
  const listener = function (event) {
    event.preventDefault();
    event.clipboardData.setData('text/html', html);
    event.clipboardData.setData('text/plain', text);
  };

  document.addEventListener('copy', listener, { once: true });
  let copied = false;
  try {
    copied = document.execCommand('copy');
  } finally {
    // Ensure listener is removed even if execCommand throws before dispatching
    // the copy event (in which case { once: true } would not have fired).
    if (!copied) {
      document.removeEventListener('copy', listener);
    }
  }
  if (!copied) {
    throw new Error('Copy command was rejected.');
  }
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    applyLocalizedText,
    clearSettingsPanelCloseTimer,
    createInsertLinksMessage,
    createRequestId,
    formatFileSize,
    getCustomServerPermissionConfirmation,
    getMessage,
    hidePermissionConfirmation,
    isRetryableRuntimeMessageError,
    persistApiBaseValue,
    RUNTIME_MESSAGE_RETRY_DELAY_MS,
    RUNTIME_MESSAGE_TIMEOUT_MS,
    scheduleSettingsPanelClose,
    sendRuntimeMessageWithRetry,
    sendRuntimeMessageWithTimeout,
    showPermissionConfirmation,
  };
}
