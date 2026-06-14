package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
)

const (
	envSlackMarkdownValidationToken                 = "SLACK_MARKDOWN_VALIDATION_BOT_TOKEN"
	envSlackMarkdownValidationChannel               = "SLACK_MARKDOWN_VALIDATION_CHANNEL"
	envSlackMarkdownValidationTeamID                = "SLACK_MARKDOWN_VALIDATION_TEAM_ID"
	envSlackMarkdownValidationEnterpriseID          = "SLACK_MARKDOWN_VALIDATION_ENTERPRISE_ID"
	envSlackMarkdownValidationTimeout               = "SLACK_MARKDOWN_VALIDATION_TIMEOUT"
	envSlackMarkdownValidationPersistentAck         = "SLACK_MARKDOWN_VALIDATION_ACK_PERSISTENT_MESSAGES"
	envSlackMarkdownValidationAssistantChannel      = "SLACK_MARKDOWN_VALIDATION_ASSISTANT_CHANNEL"
	envSlackMarkdownValidationAssistantThreadTS     = "SLACK_MARKDOWN_VALIDATION_ASSISTANT_THREAD_TS"
	envSlackMarkdownValidationAssistantRecipientID  = "SLACK_MARKDOWN_VALIDATION_ASSISTANT_RECIPIENT_TEAM_ID"
	envSlackMarkdownValidationAssistantRecipientUID = "SLACK_MARKDOWN_VALIDATION_ASSISTANT_RECIPIENT_USER_ID"
)

const defaultSlackMarkdownValidationTimeout = 5 * time.Minute

const (
	slackMarkdownValidationSubcommand = "validate-slack-markdown-renderer"

	slackMarkdownValidationReviewInstructions     = "Review the posted Slack messages against operator_check; this command does not automate renderer pass/fail."
	slackMarkdownValidationIncompleteInstructions = "Fix the delivery error and rerun validation; renderer review requires a delivered report."

	slackMarkdownValidationStatusAttempted        = "attempted"
	slackMarkdownValidationStatusSkipped          = "skipped"
	slackMarkdownValidationStatusDelivered        = "delivered"
	slackMarkdownValidationStatusConfigFailed     = "config_failed"
	slackMarkdownValidationStatusDeliveryFailed   = "delivery_failed"
	slackMarkdownValidationRendererIncomplete     = "delivery_incomplete"
	slackMarkdownValidationRendererReviewRequired = "operator_review_required"

	slackMarkdownValidationSurfaceChannelReply         = "channel_reply"
	slackMarkdownValidationSurfaceAssistantPaneStream  = "assistant_pane_stream"
	slackMarkdownValidationShapeMarkdownText           = "markdown_text"
	slackMarkdownValidationShapeStreamStart            = "stream_start"
	slackMarkdownValidationShapeStreamStop             = "stream_stop"
	slackMarkdownValidationCaseFormatting              = "formatting"
	slackMarkdownValidationCaseInlineMaskedLink        = "inline_masked_link"
	slackMarkdownValidationCaseReferenceLink           = "reference_link"
	slackMarkdownValidationCaseSlackAngleLink          = "slack_angle_link"
	slackMarkdownValidationCaseRawHTMLTagStart         = "raw_html_tag_start"
	slackMarkdownValidationCaseImageLink               = "image_link"
	slackMarkdownValidationCaseMarkdownTextRetry       = "markdown_text_compatibility_retry"
	slackMarkdownValidationShapeMarkdownBlockWithText  = "blocks[type=markdown]+text_fallback"
	slackMarkdownValidationShapePostMarkdownBlock      = "chat.postMessage blocks[type=markdown]+text_fallback"
	slackMarkdownValidationShapePostMarkdownText       = "chat.postMessage markdown_text"
	slackMarkdownValidationShapeAssistantStreamMessage = "chat.startStream+chat.appendStream markdown_text+chat.stopStream"
	slackMarkdownValidationMethodStartStream           = "chat.startStream"
	slackMarkdownValidationMethodAppendStream          = "chat.appendStream"
	slackMarkdownValidationMethodStopStream            = "chat.stopStream"

	slackMarkdownValidationAssistantChannelField         = "assistant-channel"
	slackMarkdownValidationAssistantThreadTSField        = "assistant-thread-ts"
	slackMarkdownValidationAssistantRecipientTeamIDField = "assistant-recipient-team-id"
	slackMarkdownValidationAssistantRecipientUserIDField = "assistant-recipient-user-id"
)

var slackMarkdownValidationAssistantFieldNames = []string{
	slackMarkdownValidationAssistantChannelField,
	slackMarkdownValidationAssistantThreadTSField,
	slackMarkdownValidationAssistantRecipientTeamIDField,
	slackMarkdownValidationAssistantRecipientUserIDField,
}

type slackMarkdownValidationConfig struct {
	token                    string
	channelID                string
	teamID                   string
	enterpriseID             string
	ackPersistentMessages    bool
	assistantChannelID       string
	assistantThreadTS        string
	assistantRecipientTeamID string
	assistantRecipientUserID string
	postMessageURL           string
	startStreamURL           string
	appendStreamURL          string
	stopStreamURL            string
	userAgent                string
	timeout                  time.Duration
	now                      func() time.Time
	httpClient               *http.Client
}

type slackMarkdownValidationReport struct {
	GeneratedAt        string                              `json:"generated_at"`
	Status             string                              `json:"status"`
	RendererVerdict    string                              `json:"renderer_verdict"`
	ReviewInstructions string                              `json:"review_instructions"`
	Error              string                              `json:"error,omitempty"`
	ChannelID          string                              `json:"channel_id"`
	TeamID             string                              `json:"team_id,omitempty"`
	EnterpriseID       string                              `json:"enterprise_id,omitempty"`
	Surfaces           []slackMarkdownValidationSurface    `json:"surfaces"`
	Cases              []slackMarkdownValidationCaseResult `json:"cases"`
}

type slackMarkdownValidationSurface struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type slackMarkdownValidationCaseResult struct {
	ID                string                           `json:"id"`
	Surface           string                           `json:"surface"`
	InputMarkdown     string                           `json:"input_markdown"`
	DeliveredMarkdown string                           `json:"delivered_markdown"`
	FallbackText      string                           `json:"fallback_text,omitempty"`
	RequestShape      string                           `json:"request_shape"`
	SlackTS           string                           `json:"slack_ts,omitempty"`
	Attempts          []slackMarkdownValidationAttempt `json:"attempts"`
	OperatorCheck     string                           `json:"operator_check"`
}

type slackMarkdownValidationAttempt struct {
	Method       string `json:"method"`
	RequestShape string `json:"request_shape"`
	OK           bool   `json:"ok"`
	SlackTS      string `json:"slack_ts,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	Error        string `json:"error,omitempty"`
}

type slackMarkdownValidationCase struct {
	id            string
	input         string
	operatorCheck string
}

func runSlackMarkdownRendererValidationCLI(args []string, out io.Writer) error {
	cfg, err := parseSlackMarkdownValidationConfig(args, os.Getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := runSlackMarkdownRendererValidation(ctx, &cfg)
	return writeSlackMarkdownValidationReport(out, &report, err)
}

func writeSlackMarkdownValidationReport(out io.Writer, report *slackMarkdownValidationReport, err error) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if encodeErr := enc.Encode(report); encodeErr != nil {
		if err != nil {
			return fmt.Errorf("%w; encode partial report: %w", err, encodeErr)
		}
		return encodeErr
	}
	if err != nil {
		return err
	}
	return nil
}

func parseSlackMarkdownValidationConfig(args []string, getenv func(string) string) (slackMarkdownValidationConfig, error) {
	cfg := slackMarkdownValidationConfig{
		token:                    getenv(envSlackMarkdownValidationToken),
		channelID:                getenv(envSlackMarkdownValidationChannel),
		teamID:                   getenv(envSlackMarkdownValidationTeamID),
		enterpriseID:             getenv(envSlackMarkdownValidationEnterpriseID),
		assistantChannelID:       getenv(envSlackMarkdownValidationAssistantChannel),
		assistantThreadTS:        getenv(envSlackMarkdownValidationAssistantThreadTS),
		assistantRecipientTeamID: getenv(envSlackMarkdownValidationAssistantRecipientID),
		assistantRecipientUserID: getenv(envSlackMarkdownValidationAssistantRecipientUID),
		postMessageURL:           slackChatPostMessageURL,
		startStreamURL:           slackChatStartStreamURL,
		appendStreamURL:          slackChatAppendStreamURL,
		stopStreamURL:            slackChatStopStreamURL,
		userAgent:                "qurl-slack-markdown-validator/" + version,
		timeout:                  defaultSlackMarkdownValidationTimeout,
		now:                      time.Now,
	}
	rawTimeout := strings.TrimSpace(getenv(envSlackMarkdownValidationTimeout))
	var rawTimeoutErr error
	if rawTimeout != "" {
		timeout, err := time.ParseDuration(rawTimeout)
		if err != nil {
			rawTimeoutErr = err
		} else {
			cfg.timeout = timeout
		}
	}
	rawPersistentAck := strings.TrimSpace(getenv(envSlackMarkdownValidationPersistentAck))
	var rawPersistentAckErr error
	if rawPersistentAck != "" {
		ack, err := strconv.ParseBool(rawPersistentAck)
		if err != nil {
			rawPersistentAckErr = err
		} else {
			cfg.ackPersistentMessages = ack
		}
	}
	fs := flag.NewFlagSet(slackMarkdownValidationSubcommand, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.channelID, "channel", cfg.channelID, "Slack channel id to receive channel-reply validation messages")
	fs.StringVar(&cfg.teamID, "team-id", cfg.teamID, "Slack team id for evidence metadata and token lookup")
	fs.StringVar(&cfg.enterpriseID, "enterprise-id", cfg.enterpriseID, "Slack enterprise id for evidence metadata")
	fs.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "overall live validation timeout")
	fs.BoolVar(&cfg.ackPersistentMessages, "ack-persistent-messages", cfg.ackPersistentMessages, "acknowledge validation posts persistent Slack evidence messages")
	fs.StringVar(&cfg.assistantChannelID, "assistant-channel", cfg.assistantChannelID, "assistant-pane channel id for chat.startStream validation")
	fs.StringVar(&cfg.assistantThreadTS, "assistant-thread-ts", cfg.assistantThreadTS, "assistant-pane thread ts for chat.startStream validation")
	fs.StringVar(&cfg.assistantRecipientTeamID, "assistant-recipient-team-id", cfg.assistantRecipientTeamID, "recipient team id for chat.startStream validation")
	fs.StringVar(&cfg.assistantRecipientUserID, "assistant-recipient-user-id", cfg.assistantRecipientUserID, "recipient user id for chat.startStream validation")
	if err := fs.Parse(args); err != nil {
		return slackMarkdownValidationConfig{}, err
	}
	timeoutFlagSet := false
	persistentAckFlagSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			timeoutFlagSet = true
		}
		if f.Name == "ack-persistent-messages" {
			persistentAckFlagSet = true
		}
	})
	if rawTimeoutErr != nil && !timeoutFlagSet {
		return slackMarkdownValidationConfig{}, fmt.Errorf("%s must be a Go duration: %w", envSlackMarkdownValidationTimeout, rawTimeoutErr)
	}
	if rawPersistentAckErr != nil && !persistentAckFlagSet {
		return slackMarkdownValidationConfig{}, fmt.Errorf("%s must be a boolean: %w", envSlackMarkdownValidationPersistentAck, rawPersistentAckErr)
	}
	cfg.token = strings.TrimSpace(cfg.token)
	cfg.channelID = strings.TrimSpace(cfg.channelID)
	cfg.teamID = strings.TrimSpace(cfg.teamID)
	cfg.enterpriseID = strings.TrimSpace(cfg.enterpriseID)
	cfg.assistantChannelID = strings.TrimSpace(cfg.assistantChannelID)
	cfg.assistantThreadTS = strings.TrimSpace(cfg.assistantThreadTS)
	cfg.assistantRecipientTeamID = strings.TrimSpace(cfg.assistantRecipientTeamID)
	cfg.assistantRecipientUserID = strings.TrimSpace(cfg.assistantRecipientUserID)
	if err := validateSlackMarkdownValidationRequiredConfig(&cfg); err != nil {
		return slackMarkdownValidationConfig{}, err
	}
	return cfg, nil
}

func runSlackMarkdownRendererValidation(ctx context.Context, input *slackMarkdownValidationConfig) (report slackMarkdownValidationReport, err error) {
	failureStatus := slackMarkdownValidationStatusDeliveryFailed
	defer func() {
		if err != nil {
			report.Status = failureStatus
			report.Error = err.Error()
			if failureStatus == slackMarkdownValidationStatusDeliveryFailed {
				report.RendererVerdict = slackMarkdownValidationRendererIncomplete
				report.ReviewInstructions = slackMarkdownValidationIncompleteInstructions
			}
		}
	}()
	cfg := *input
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.httpClient == nil {
		cfg.httpClient = defaultSlackPostMessageClient()
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultSlackMarkdownValidationTimeout
	}
	// The parser already trims CLI configs; normalize this copy so direct
	// test/operator entry points get the same validation without mutating input.
	normalizeSlackMarkdownValidationConfig(&cfg)
	cases := slackMarkdownRendererValidationCases()
	report = slackMarkdownValidationReport{
		GeneratedAt:        cfg.now().UTC().Format(time.RFC3339),
		Status:             slackMarkdownValidationStatusDelivered,
		RendererVerdict:    slackMarkdownValidationRendererReviewRequired,
		ReviewInstructions: slackMarkdownValidationReviewInstructions,
		ChannelID:          cfg.channelID,
		TeamID:             cfg.teamID,
		EnterpriseID:       cfg.enterpriseID,
	}
	// CLI and direct entry points share validation so required fields cannot
	// drift. Direct config errors intentionally produce config_failed JSON; CLI
	// config errors return before report construction so operator runs emit none.
	if err := validateSlackMarkdownValidationRequiredConfig(&cfg); err != nil {
		failureStatus = slackMarkdownValidationStatusConfigFailed
		report.RendererVerdict = ""
		report.ReviewInstructions = ""
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	report.Surfaces = []slackMarkdownValidationSurface{
		{Name: slackMarkdownValidationSurfaceChannelReply, Status: slackMarkdownValidationStatusAttempted},
	}

	var threadTS string
	postMessagePoster := newSlackMarkdownValidationPostMessagePoster(&cfg)
	for _, tc := range cases {
		result, err := postSlackMarkdownValidationCase(ctx, &cfg, postMessagePoster, tc, threadTS)
		report.Cases = append(report.Cases, result)
		if err != nil {
			report.Surfaces[0].Status = slackMarkdownValidationStatusDeliveryFailed
			return report, err
		}
		if threadTS == "" {
			threadTS = result.SlackTS
		}
	}
	compat, err := postSlackMarkdownCompatibilityValidationCase(ctx, &cfg, postMessagePoster, threadTS)
	report.Cases = append(report.Cases, compat)
	if err != nil {
		report.Surfaces[0].Status = slackMarkdownValidationStatusDeliveryFailed
		return report, err
	}
	report.Surfaces[0].Status = slackMarkdownValidationStatusDelivered

	assistantSurface := slackMarkdownValidationSurface{Name: slackMarkdownValidationSurfaceAssistantPaneStream}
	missingAssistantFields := slackMarkdownValidationMissingAssistantFields(&cfg)
	if len(missingAssistantFields) > 0 {
		assistantSurface.Reason = assistantValidationSkipReason(&cfg)
		assistantSurface.Status = slackMarkdownValidationStatusSkipped
		report.Surfaces = append(report.Surfaces, assistantSurface)
		return report, nil
	}
	assistantSurface.Status = slackMarkdownValidationStatusAttempted
	assistantSurfaceIndex := len(report.Surfaces)
	report.Surfaces = append(report.Surfaces, assistantSurface)
	streamPort := newSlackMarkdownValidationStreamPort(&cfg)
	for _, tc := range cases {
		result, err := streamSlackMarkdownValidationCaseWithPort(ctx, &cfg, streamPort, tc)
		report.Cases = append(report.Cases, result)
		if err != nil {
			report.Surfaces[assistantSurfaceIndex].Status = slackMarkdownValidationStatusDeliveryFailed
			return report, err
		}
	}
	report.Surfaces[assistantSurfaceIndex].Status = slackMarkdownValidationStatusDelivered
	return report, nil
}

func normalizeSlackMarkdownValidationConfig(cfg *slackMarkdownValidationConfig) {
	cfg.token = strings.TrimSpace(cfg.token)
	cfg.channelID = strings.TrimSpace(cfg.channelID)
	cfg.teamID = strings.TrimSpace(cfg.teamID)
	cfg.enterpriseID = strings.TrimSpace(cfg.enterpriseID)
	cfg.assistantChannelID = strings.TrimSpace(cfg.assistantChannelID)
	cfg.assistantThreadTS = strings.TrimSpace(cfg.assistantThreadTS)
	cfg.assistantRecipientTeamID = strings.TrimSpace(cfg.assistantRecipientTeamID)
	cfg.assistantRecipientUserID = strings.TrimSpace(cfg.assistantRecipientUserID)
}

func validateSlackMarkdownValidationRequiredConfig(cfg *slackMarkdownValidationConfig) error {
	if cfg.token == "" {
		return fmt.Errorf("%s is required", envSlackMarkdownValidationToken)
	}
	if err := auth.ValidateSlackBotTokenShape(cfg.token); err != nil {
		return err
	}
	if cfg.channelID == "" {
		return fmt.Errorf("%s or --channel is required", envSlackMarkdownValidationChannel)
	}
	if cfg.timeout <= 0 {
		return fmt.Errorf("%s or --timeout must be greater than zero", envSlackMarkdownValidationTimeout)
	}
	if !cfg.ackPersistentMessages {
		return fmt.Errorf("%s=true or --ack-persistent-messages is required because validation posts persistent Slack messages", envSlackMarkdownValidationPersistentAck)
	}
	return validateSlackMarkdownValidationAssistantConfig(cfg)
}

func validateSlackMarkdownValidationAssistantConfig(cfg *slackMarkdownValidationConfig) error {
	missing := slackMarkdownValidationMissingAssistantFields(cfg)
	if len(missing) == 0 || len(missing) == len(slackMarkdownValidationAssistantFieldNames) {
		return nil
	}
	return errors.New(assistantValidationSkipReason(cfg))
}

func assistantValidationSkipReason(cfg *slackMarkdownValidationConfig) string {
	missing := slackMarkdownValidationMissingAssistantFields(cfg)
	if len(missing) == len(slackMarkdownValidationAssistantFieldNames) {
		return "set " + strings.Join(slackMarkdownValidationAssistantFieldNames, ", ") + " to validate chat.startStream/chat.appendStream"
	}
	return "partial assistant-pane config; missing " + strings.Join(missing, ", ")
}

type slackMarkdownValidationAssistantField struct {
	name  string
	value string
}

func slackMarkdownValidationAssistantFields(cfg *slackMarkdownValidationConfig) []slackMarkdownValidationAssistantField {
	return []slackMarkdownValidationAssistantField{
		{name: slackMarkdownValidationAssistantChannelField, value: cfg.assistantChannelID},
		{name: slackMarkdownValidationAssistantThreadTSField, value: cfg.assistantThreadTS},
		{name: slackMarkdownValidationAssistantRecipientTeamIDField, value: cfg.assistantRecipientTeamID},
		{name: slackMarkdownValidationAssistantRecipientUserIDField, value: cfg.assistantRecipientUserID},
	}
}

func slackMarkdownValidationMissingAssistantFields(cfg *slackMarkdownValidationConfig) []string {
	fields := slackMarkdownValidationAssistantFields(cfg)
	missing := make([]string, 0, len(fields))
	for _, field := range fields {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	return missing
}

func slackMarkdownRendererValidationCases() []slackMarkdownValidationCase {
	return []slackMarkdownValidationCase{
		{
			id:            slackMarkdownValidationCaseFormatting,
			input:         "Renderer validation: **bold**, bullets:\n- first item\n- second item\nand inline `code`.",
			operatorCheck: "Slack should render bold text, bullets, and inline code without changing the visible words.",
		},
		{
			id:            slackMarkdownValidationCaseInlineMaskedLink,
			input:         "Inline masked link: [billing portal](https://example.com/billing/login).",
			operatorCheck: "Slack must show both the label and destination, not a hidden-destination link.",
		},
		{
			id:            slackMarkdownValidationCaseReferenceLink,
			input:         "Reference link: [billing portal][billing].\n\n[billing]: https://example.com/billing/login",
			operatorCheck: "Slack must not resolve the reference into a hidden-destination link.",
		},
		{
			id:            slackMarkdownValidationCaseSlackAngleLink,
			input:         "Slack angle link: <https://example.com/billing/login|billing portal>.",
			operatorCheck: "Slack must show both the angle-link label and destination.",
		},
		{
			id:            slackMarkdownValidationCaseRawHTMLTagStart,
			input:         `HTML tag start: <a href="https://example.com/billing/login">billing portal</a>.`,
			operatorCheck: "Slack must render the tag text literally and must not create a hidden HTML link.",
		},
		{
			id:            slackMarkdownValidationCaseImageLink,
			input:         "Image syntax: ![billing screenshot](https://example.com/billing/screen.png).",
			operatorCheck: "Slack must show the image label and destination instead of an image-only hidden destination.",
		},
	}
}

func postSlackMarkdownValidationCase(ctx context.Context, cfg *slackMarkdownValidationConfig, poster *slackWebAPIPoster, tc slackMarkdownValidationCase, threadTS string) (slackMarkdownValidationCaseResult, error) {
	body, err := slackMarkdownBlockMessageBody(cfg.channelID, threadTS, tc.input)
	if err != nil {
		return slackMarkdownValidationCaseResult{}, fmt.Errorf("%s markdown block body: %w", tc.id, err)
	}
	delivered, fallbackText, err := slackValidationMarkdownBlockBodyEvidence(body)
	if err != nil {
		return slackMarkdownValidationCaseResult{}, fmt.Errorf("%s markdown block body evidence: %w", tc.id, err)
	}
	result := slackMarkdownValidationCaseResult{
		ID:                tc.id,
		Surface:           slackMarkdownValidationSurfaceChannelReply,
		InputMarkdown:     tc.input,
		DeliveredMarkdown: delivered,
		FallbackText:      fallbackText,
		RequestShape:      slackMarkdownValidationShapePostMarkdownBlock,
		OperatorCheck:     tc.operatorCheck,
	}
	attempt, err := sendSlackValidationPayload(ctx, cfg, poster, slackMarkdownValidationShapeMarkdownBlockWithText, body)
	result.Attempts = append(result.Attempts, attempt)
	if err == nil {
		if attempt.SlackTS == "" {
			return result, fmt.Errorf("%s markdown block post: missing Slack ts", tc.id)
		}
		result.SlackTS = attempt.SlackTS
		return result, nil
	}
	if !isSlackMarkdownBlockFallbackError(err) {
		return result, fmt.Errorf("%s markdown block post: %w", tc.id, err)
	}
	retryBody, retryErr := slackMarkdownTextMessageBody(cfg.channelID, threadTS, tc.input)
	if retryErr != nil {
		return result, fmt.Errorf("%s markdown_text body: %w", tc.id, retryErr)
	}
	deliveredRetry, retryErr := slackValidationMarkdownTextBodyText(retryBody)
	if retryErr != nil {
		return result, fmt.Errorf("%s markdown_text body evidence: %w", tc.id, retryErr)
	}
	// Keep this fallback trigger in step with newSlackPostMarkdownMessageFuncWithTokenLookup.
	// The validator owns orchestration so the JSON report can preserve each
	// attempt's ts/error_code while still sharing the production trigger and bodies.
	result.RequestShape = slackMarkdownValidationShapePostMarkdownText
	result.DeliveredMarkdown = deliveredRetry
	result.FallbackText = ""
	retryAttempt, retryErr := sendSlackValidationPayload(ctx, cfg, poster, slackMarkdownValidationShapeMarkdownText, retryBody)
	result.Attempts = append(result.Attempts, retryAttempt)
	if retryErr != nil {
		return result, fmt.Errorf("%s markdown_text retry: %w", tc.id, retryErr)
	}
	if retryAttempt.SlackTS == "" {
		return result, fmt.Errorf("%s markdown_text retry: missing Slack ts", tc.id)
	}
	result.SlackTS = retryAttempt.SlackTS
	return result, nil
}

func postSlackMarkdownCompatibilityValidationCase(ctx context.Context, cfg *slackMarkdownValidationConfig, poster *slackWebAPIPoster, threadTS string) (slackMarkdownValidationCaseResult, error) {
	tc := slackMarkdownValidationCase{
		id:            slackMarkdownValidationCaseMarkdownTextRetry,
		input:         "Compatibility retry body: **bold** and [billing portal](https://example.com/billing/login).",
		operatorCheck: "Slack should accept the markdown_text-only compatibility body; this is the body used if markdown blocks are rejected.",
	}
	body, err := slackMarkdownTextMessageBody(cfg.channelID, threadTS, tc.input)
	if err != nil {
		return slackMarkdownValidationCaseResult{}, fmt.Errorf("%s body: %w", tc.id, err)
	}
	result := slackMarkdownValidationCaseResult{
		ID:            tc.id,
		Surface:       slackMarkdownValidationSurfaceChannelReply,
		InputMarkdown: tc.input,
		RequestShape:  slackMarkdownValidationShapePostMarkdownText,
		OperatorCheck: tc.operatorCheck,
	}
	result.DeliveredMarkdown, err = slackValidationMarkdownTextBodyText(body)
	if err != nil {
		return result, fmt.Errorf("%s body evidence: %w", tc.id, err)
	}
	attempt, err := sendSlackValidationPayload(ctx, cfg, poster, slackMarkdownValidationShapeMarkdownText, body)
	result.Attempts = append(result.Attempts, attempt)
	if err != nil {
		return result, fmt.Errorf("%s post: %w", tc.id, err)
	}
	if attempt.SlackTS == "" {
		return result, fmt.Errorf("%s post: missing Slack ts", tc.id)
	}
	result.SlackTS = attempt.SlackTS
	return result, nil
}

func newSlackMarkdownValidationStreamPort(cfg *slackMarkdownValidationConfig) internal.AgentStreamPort {
	return newSlackAgentStreamPortWithTokenLookup(staticSlackMarkdownValidationTokenLookup(cfg.token), cfg.userAgent, cfg.startStreamURL, cfg.appendStreamURL, cfg.stopStreamURL, cfg.httpClient)
}

func newSlackMarkdownValidationPostMessagePoster(cfg *slackMarkdownValidationConfig) *slackWebAPIPoster {
	return newSlackWebAPIPoster(staticSlackMarkdownValidationTokenLookup(cfg.token), cfg.userAgent, cfg.postMessageURL, "chat.postMessage", slackChatPostMessageResponseError, cfg.httpClient)
}

func streamSlackMarkdownValidationCaseWithPort(ctx context.Context, cfg *slackMarkdownValidationConfig, port internal.AgentStreamPort, tc slackMarkdownValidationCase) (slackMarkdownValidationCaseResult, error) {
	start := &internal.AgentStreamStart{
		TeamID:          cfg.teamID,
		EnterpriseID:    cfg.enterpriseID,
		ChannelID:       cfg.assistantChannelID,
		ThreadTS:        cfg.assistantThreadTS,
		RecipientTeamID: cfg.assistantRecipientTeamID,
		RecipientUserID: cfg.assistantRecipientUserID,
	}
	streamTS, err := port.StartStream(ctx, start)
	delivered := internal.HardenAgentMarkdownStream(tc.input)
	startAttempt := slackMarkdownValidationAttempt{
		Method:       slackMarkdownValidationMethodStartStream,
		RequestShape: slackMarkdownValidationShapeStreamStart,
		OK:           err == nil,
	}
	recordSlackValidationAttemptError(&startAttempt, err)
	result := slackMarkdownValidationCaseResult{
		ID:                tc.id,
		Surface:           slackMarkdownValidationSurfaceAssistantPaneStream,
		InputMarkdown:     tc.input,
		DeliveredMarkdown: delivered,
		RequestShape:      slackMarkdownValidationShapeAssistantStreamMessage,
		OperatorCheck:     tc.operatorCheck,
		Attempts:          []slackMarkdownValidationAttempt{startAttempt},
	}
	if err != nil {
		return result, fmt.Errorf("%s start stream: %w", tc.id, err)
	}
	if streamTS == "" {
		// No stream handle exists, so there is nothing safe to stop; fail before
		// append instead of sending a chunk with an empty stream identifier.
		return result, fmt.Errorf("%s start stream: missing Slack ts", tc.id)
	}
	result.SlackTS = streamTS
	result.Attempts[0].SlackTS = streamTS
	if err := port.AppendStream(ctx, cfg.teamID, cfg.enterpriseID, cfg.assistantChannelID, streamTS, result.DeliveredMarkdown); err != nil {
		attempt := slackMarkdownValidationAttempt{Method: slackMarkdownValidationMethodAppendStream, RequestShape: slackMarkdownValidationShapeMarkdownText, OK: false}
		recordSlackValidationAttemptError(&attempt, err)
		result.Attempts = append(result.Attempts, attempt)
		stopAttempt := slackMarkdownValidationAttempt{Method: slackMarkdownValidationMethodStopStream, RequestShape: slackMarkdownValidationShapeStreamStop}
		stopErr := port.StopStream(ctx, cfg.teamID, cfg.enterpriseID, cfg.assistantChannelID, streamTS)
		stopAttempt.OK = stopErr == nil
		recordSlackValidationAttemptError(&stopAttempt, stopErr)
		result.Attempts = append(result.Attempts, stopAttempt)
		return result, fmt.Errorf("%s append stream: %w", tc.id, err)
	}
	result.Attempts = append(result.Attempts, slackMarkdownValidationAttempt{Method: slackMarkdownValidationMethodAppendStream, RequestShape: slackMarkdownValidationShapeMarkdownText, OK: true})
	if err := port.StopStream(ctx, cfg.teamID, cfg.enterpriseID, cfg.assistantChannelID, streamTS); err != nil {
		attempt := slackMarkdownValidationAttempt{Method: slackMarkdownValidationMethodStopStream, RequestShape: slackMarkdownValidationShapeStreamStop, OK: false}
		recordSlackValidationAttemptError(&attempt, err)
		result.Attempts = append(result.Attempts, attempt)
		return result, fmt.Errorf("%s stop stream: %w", tc.id, err)
	}
	result.Attempts = append(result.Attempts, slackMarkdownValidationAttempt{Method: slackMarkdownValidationMethodStopStream, RequestShape: slackMarkdownValidationShapeStreamStop, OK: true})
	return result, nil
}

func sendSlackValidationPayload(ctx context.Context, cfg *slackMarkdownValidationConfig, poster *slackWebAPIPoster, requestShape string, body []byte) (slackMarkdownValidationAttempt, error) {
	attempt := slackMarkdownValidationAttempt{Method: poster.op, RequestShape: requestShape}
	raw, err := poster.gridPost(ctx, cfg.teamID, cfg.enterpriseID, body)
	recordSlackValidationAttemptError(&attempt, err)
	if err != nil {
		return attempt, err
	}
	var out struct {
		TS      string `json:"ts"`
		Message struct {
			TS string `json:"ts"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		decodeErr := fmt.Errorf("%s validation response decode: %w", poster.op, err)
		recordSlackValidationAttemptError(&attempt, decodeErr)
		return attempt, decodeErr
	}
	attempt.OK = true
	if out.TS != "" {
		attempt.SlackTS = out.TS
	} else {
		attempt.SlackTS = out.Message.TS
	}
	return attempt, nil
}

func slackValidationMarkdownBlockBodyEvidence(body []byte) (deliveredMarkdown, fallbackText string, err error) {
	var payload struct {
		Blocks []slackMarkdownBlock `json:"blocks"`
		Text   string               `json:"text"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", err
	}
	// Keep in step with slackMarkdownBlockMessageBody: the evidence artifact is
	// intentionally tied to the single markdown block body sent on the wire.
	if len(payload.Blocks) != 1 || payload.Blocks[0].Text == "" {
		return "", "", errors.New("missing markdown block text")
	}
	return payload.Blocks[0].Text, payload.Text, nil
}

func slackValidationMarkdownTextBodyText(body []byte) (string, error) {
	var payload struct {
		MarkdownText string `json:"markdown_text"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.MarkdownText == "" {
		return "", errors.New("missing markdown_text")
	}
	return payload.MarkdownText, nil
}

func recordSlackValidationAttemptError(attempt *slackMarkdownValidationAttempt, err error) {
	if err == nil {
		return
	}
	attempt.Error = err.Error()
	attempt.ErrorCode = slackValidationErrorCode(err)
}

func slackValidationErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if code := slackChatPostMessageErrorCode(err); code != "" {
		return code
	}
	var apiErr *slackWebAPIError
	if errors.As(err, &apiErr) {
		return apiErr.code
	}
	return ""
}

func staticSlackMarkdownValidationTokenLookup(token string) slackBotTokenLookup {
	return func(context.Context, string) (string, error) { return token, nil }
}
