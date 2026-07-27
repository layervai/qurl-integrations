package internal

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// Activity is the Bot Framework activity payload delivered by Teams.
type Activity struct {
	Type         string              `json:"type"`
	ID           string              `json:"id,omitempty"`
	ServiceURL   string              `json:"serviceUrl,omitempty"`
	ChannelID    string              `json:"channelId,omitempty"`
	Text         string              `json:"text,omitempty"`
	TextFormat   string              `json:"textFormat,omitempty"`
	ReplyToID    string              `json:"replyToId,omitempty"`
	From         ChannelAccount      `json:"from"`
	Recipient    ChannelAccount      `json:"recipient"`
	Conversation ConversationAccount `json:"conversation"`
	Entities     []Entity            `json:"entities,omitempty"`
	ChannelData  ChannelData         `json:"channelData,omitempty"`
}

// ChannelAccount identifies a Teams user or bot account.
type ChannelAccount struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	AadObjectID string `json:"aadObjectId,omitempty"`
}

// ConversationAccount describes the Teams conversation carrying the activity.
type ConversationAccount struct {
	ID               string `json:"id,omitempty"`
	Name             string `json:"name,omitempty"`
	ConversationType string `json:"conversationType,omitempty"`
	TenantID         string `json:"tenantId,omitempty"`
	IsGroup          bool   `json:"isGroup,omitempty"`
}

// Entity carries rich Teams activity entities such as mentions.
type Entity struct {
	Type      string         `json:"type,omitempty"`
	Text      string         `json:"text,omitempty"`
	Mentioned ChannelAccount `json:"mentioned,omitempty"`
}

// ChannelData contains Teams tenant, team, and channel identifiers.
type ChannelData struct {
	Tenant struct {
		ID string `json:"id,omitempty"`
	} `json:"tenant,omitempty"`
	Team struct {
		ID string `json:"id,omitempty"`
	} `json:"team,omitempty"`
	Channel struct {
		ID string `json:"id,omitempty"`
	} `json:"channel,omitempty"`
}

var teamsHTMLTagPattern = regexp.MustCompile(`(?is)<[^>]+>`)

func normalizeActivityText(a *Activity) string {
	if a == nil {
		return ""
	}
	text := a.Text
	mentionPlaceholders := map[string]string{}
	mentionIndex := 0
	for i := range a.Entities {
		entity := &a.Entities[i]
		if !strings.EqualFold(strings.TrimSpace(entity.Type), "mention") || strings.TrimSpace(entity.Text) == "" {
			continue
		}
		replacement := ""
		if !isBotMention(a, entity) {
			mentionIndex++
			sentinel := "__qurl_teams_mention_" + strconv.Itoa(mentionIndex) + "__"
			replacement = sentinel
			mentionPlaceholders[sentinel] = "<@" + strings.TrimSpace(entity.Mentioned.ID) + ">"
		}
		text = strings.ReplaceAll(text, entity.Text, replacement)
	}
	text = strings.NewReplacer(
		"<br>", " ",
		"<br/>", " ",
		"<br />", " ",
		"</div>", " ",
		"<div>", " ",
		"&nbsp;", " ",
	).Replace(text)
	text = html.UnescapeString(text)
	text = teamsHTMLTagPattern.ReplaceAllString(text, " ")
	for sentinel, mention := range mentionPlaceholders {
		text = strings.ReplaceAll(text, sentinel, mention)
	}
	return strings.Join(strings.Fields(text), " ")
}

func isBotMention(a *Activity, entity *Entity) bool {
	if a == nil {
		return false
	}
	if entity == nil {
		return false
	}
	mentionedID := strings.TrimSpace(entity.Mentioned.ID)
	if mentionedID == "" {
		return false
	}
	return mentionedID == strings.TrimSpace(a.Recipient.ID) || mentionedID == strings.TrimSpace(a.Recipient.AadObjectID)
}

type scopeInfo struct {
	TenantID     string
	ScopeID      string
	ScopeLabel   string
	Personal     bool
	Channel      bool
	GroupChat    bool
	Conversation string
}

func deriveScope(a *Activity) scopeInfo {
	var scope scopeInfo
	if a == nil {
		return scope
	}
	scope.TenantID = strings.TrimSpace(a.ChannelData.Tenant.ID)
	if scope.TenantID == "" {
		scope.TenantID = strings.TrimSpace(a.Conversation.TenantID)
	}
	scope.Conversation = strings.TrimSpace(a.Conversation.ID)
	switch strings.ToLower(strings.TrimSpace(a.Conversation.ConversationType)) {
	case "personal":
		scope.Personal = true
		scope.ScopeLabel = "personal chat"
	case "channel":
		scope.Channel = true
		scope.ScopeID = strings.TrimSpace(a.ChannelData.Channel.ID)
		if scope.ScopeID == "" {
			scope.ScopeID = scope.Conversation
		}
		scope.ScopeLabel = "channel"
	case "groupchat":
		scope.GroupChat = true
		scope.ScopeLabel = "group chat"
	}
	return scope
}
