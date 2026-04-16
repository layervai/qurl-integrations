/**
 * Central selector registry for Discord web app elements.
 * All Playwright selectors live here so tests stay resilient to Discord UI changes.
 */

export const DiscordSelectors = {
  // Login page
  login: {
    emailInput: 'input[name="email"]',
    passwordInput: 'input[name="password"]',
    loginButton: 'button[type="submit"]',
    totpInput: 'input[placeholder="6-digit authentication code"]',
    totpSubmit: 'button[type="submit"]',
    captchaFrame: 'iframe[src*="captcha"]',
    appLoaded: 'nav[aria-label*="Servers sidebar"]',
  },

  // Channel / server
  channel: {
    messageInput: '[role="textbox"][data-slate-editor="true"]',
    messageList: '[data-list-id="chat-messages"]',
    messageItem: '[id^="chat-messages-"]',
    messageContent: '[class*="messageContent-"]',
    channelHeader: '[class*="title-"]',
    membersList: '[class*="members-"]',
    serverSidebar: '[class*="sidebar-"]',
    channelLink: (name: string) => `a[data-list-item-id*="channels"] >> text="${name}"`,
    serverIcon: (name: string) => `[data-dnd-name="${name}"]`,
  },

  // Slash commands
  slashCommand: {
    commandOption: '[class*="autocomplete-"] [role="option"]',
    commandName: '[class*="commandName-"]',
    optionInput: '[class*="optionInput-"]',
    autocompleteRow: '[class*="autocompleteRowContent-"]',
    autocompleteList: '[class*="autocomplete-"]',
  },

  // Embeds
  embed: {
    container: '[class*="embedWrapper-"]',
    title: '[class*="embedTitle-"]',
    description: '[class*="embedDescription-"]',
    field: '[class*="embedField-"]',
    fieldName: '[class*="embedFieldName-"]',
    fieldValue: '[class*="embedFieldValue-"]',
    footer: '[class*="embedFooter-"]',
    thumbnail: '[class*="embedThumbnail-"]',
    image: '[class*="embedImage-"]',
  },

  // Buttons / interactions
  interaction: {
    button: '[class*="component-"] button',
    buttonByLabel: (label: string) => `button:has-text("${label}")`,
    selectMenu: '[class*="component-"] [class*="select-"]',
    selectOption: '[role="option"]',
    actionRow: '[class*="actionButtons-"]',
  },

  // Modals
  modal: {
    container: '[class*="modal-"]',
    root: '[class*="root-"][role="dialog"]',
    header: '[class*="header-"]',
    textInput: '[class*="modal-"] input[type="text"], [class*="modal-"] textarea',
    submitButton: '[class*="modal-"] button[type="submit"]',
    cancelButton: '[class*="modal-"] button:has-text("Cancel")',
    closeButton: '[class*="closeButton-"]',
  },

  // DMs
  dm: {
    container: '[class*="privateChannels-"]',
    dmLink: (username: string) => `[class*="privateChannels-"] a:has-text("${username}")`,
    newDmButton: '[aria-label="Direct Messages"] ~ [class*="header-"] a',
    searchInput: '[class*="quickswitcher-"] input',
    messageGroup: '[class*="message-"]',
  },

  // User picker
  userPicker: {
    searchInput: 'input[placeholder*="Search"]',
    userOption: '[role="option"]',
    userTag: '[class*="tag-"]',
    selectedUser: '[class*="selected-"]',
  },

  // Voice
  voice: {
    joinButton: '[aria-label*="voice channel"]',
    disconnectButton: 'button[aria-label="Disconnect"]',
    voiceConnected: '[class*="rtcConnectionStatus-"]',
    videoButton: 'button[aria-label*="Camera"]',
    muteButton: 'button[aria-label*="Mute"]',
  },

  // Ephemeral / system messages
  ephemeral: {
    ephemeralMarker: ':has-text("Only you can see this")',
    ephemeralMessage: '[class*="ephemeral-"]',
    dismissButton: 'button:has-text("Dismiss message")',
  },

  // Misc
  misc: {
    tooltip: '[class*="tooltip-"]',
    popout: '[class*="popout-"]',
    layerContainer: '[class*="layerContainer-"]',
    loadingIndicator: '[class*="loading-"]',
    notice: '[class*="notice-"]',
  },
} as const;

/** ARIA role-based selectors for accessibility-focused queries */
export const Roles = {
  textbox: 'role=textbox',
  button: (name: string) => `role=button[name="${name}"]`,
  link: (name: string) => `role=link[name="${name}"]`,
  listitem: 'role=listitem',
  option: (name: string) => `role=option[name="${name}"]`,
  dialog: 'role=dialog',
  tab: (name: string) => `role=tab[name="${name}"]`,
  heading: (level: number) => `role=heading[level=${level}]`,
} as const;
