package internal

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	callbackIDConnectorSetup   = "connector_setup"
	callbackIDS3WebsiteInstall = "s3_website_install"

	connectorSetupBlockType  = "connector_setup_type"
	connectorSetupActionType = "connector_setup_type_select"

	connectorSetupExistingService = "existing_service"
	connectorSetupS3Website       = "s3_website"

	s3WebsiteInstallBlockSlug         = "s3_connector_slug"
	s3WebsiteInstallActionSlug        = "s3_connector_slug_input"
	s3WebsiteInstallBlockShortcut     = "s3_channel_shortcut"
	s3WebsiteInstallActionShortcut    = "s3_channel_shortcut_input"
	s3WebsiteInstallBlockEnvironment  = "s3_target_environment"
	s3WebsiteInstallActionEnvironment = "s3_target_environment_select"
	s3WebsiteInstallBlockBucket       = "s3_bucket"
	s3WebsiteInstallActionBucket      = "s3_bucket_input"
	s3WebsiteInstallBlockRegion       = "s3_region"
	s3WebsiteInstallActionRegion      = "s3_region_input"
	s3WebsiteInstallBlockPrefix       = "s3_prefix"
	s3WebsiteInstallActionPrefix      = "s3_prefix_input"
	s3WebsiteInstallBlockIndex        = "s3_index_document"
	s3WebsiteInstallActionIndex       = "s3_index_document_input"
)

// ConnectorSetupModal is the qURL Connector branch point opened from
// `/qurl-admin protect` -> "Protect qURL Connector". Existing-service setup
// routes to the long-standing qURL Connector installer; S3 hosted website setup
// routes to a separate modal that collects bucket details and renders both the
// qURL Connector and private S3 origin artifacts.
func ConnectorSetupModal(meta *TunnelInstallModalMetadata) ([]byte, error) {
	if meta == nil {
		return nil, errors.New("connector setup modal metadata is missing")
	}
	privateMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	if len(privateMeta) > slackPrivateMetadataMaxBytes {
		return nil, fmt.Errorf("private_metadata exceeds Slack limit: %d bytes", len(privateMeta))
	}
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDConnectorSetup,
		blockKitFieldTitle:           plainTextObj("Set up qURL Connector"),
		blockKitFieldSubmit:          plainTextObj("Continue"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Target channel: " + slackChannelMention(meta.ChannelID)),
			inputBlock(connectorSetupBlockType, "What are you protecting?", "Choose the closest setup type. The next screen asks only for the details that setup needs.", false,
				staticSelect(connectorSetupActionType, []map[string]any{
					optionObj("Existing service", connectorSetupExistingService),
					optionObj("S3 hosted website", connectorSetupS3Website),
				}, optionObj("Existing service", connectorSetupExistingService))),
		},
	}
	return json.Marshal(payload)
}

// S3WebsiteInstallModal renders the qURL Connector setup form for private S3
// static websites. It keeps the existing-service connector install separate so
// users protecting a local service do not have to reason about S3-only fields.
func S3WebsiteInstallModal(meta *TunnelInstallModalMetadata) ([]byte, error) {
	if meta == nil {
		return nil, errors.New("S3 website install modal metadata is missing")
	}
	privateMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	if len(privateMeta) > slackPrivateMetadataMaxBytes {
		return nil, fmt.Errorf("private_metadata exceeds Slack limit: %d bytes", len(privateMeta))
	}
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDS3WebsiteInstall,
		blockKitFieldTitle:           plainTextObj("Protect S3 Website"),
		blockKitFieldSubmit:          plainTextObj("Generate"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Target channel: " + slackChannelMention(meta.ChannelID)),
			inputBlock(s3WebsiteInstallBlockSlug, "qURL Connector ID", "3-64 lowercase letters, numbers, and hyphens. Start with a letter, end with a letter or number.", false,
				plainTextInput(s3WebsiteInstallActionSlug, "stats-site", "")),
			inputBlock(s3WebsiteInstallBlockShortcut, "Channel alias", "Optional. Leave blank to use the qURL Connector ID.", true,
				plainTextInput(s3WebsiteInstallActionShortcut, "stats", "")),
			inputBlock(s3WebsiteInstallBlockEnvironment, "Target environment", "Choose where the qURL Connector and private S3 origin will run.", false,
				staticSelect(s3WebsiteInstallActionEnvironment, []map[string]any{
					optionObj("Docker host", string(tunnelEnvDocker)),
					optionObj("Docker Compose", string(tunnelEnvCompose)),
					optionObj("AWS ECS/Fargate task", string(tunnelEnvECSFargate)),
					optionObj("Kubernetes pod", string(tunnelEnvKubernetes)),
				}, optionObj("Docker host", string(tunnelEnvDocker)))),
			inputBlock(s3WebsiteInstallBlockBucket, "S3 bucket", "Private bucket name. Dotted bucket names are not supported by this origin image.", false,
				plainTextInput(s3WebsiteInstallActionBucket, "my-static-site", "")),
			inputBlock(s3WebsiteInstallBlockRegion, "AWS region", "Commercial AWS region for the bucket, for example us-east-1.", false,
				plainTextInput(s3WebsiteInstallActionRegion, "us-east-1", "")),
			inputBlock(s3WebsiteInstallBlockPrefix, "S3 prefix", "Optional. Use this when the website files live under a folder such as website.", true,
				plainTextInput(s3WebsiteInstallActionPrefix, "website", "")),
			inputBlock(s3WebsiteInstallBlockIndex, "Index document", "Usually index.html.", false,
				plainTextInput(s3WebsiteInstallActionIndex, "index.html", "index.html")),
		},
	}
	return json.Marshal(payload)
}

// S3WebsiteInstallErrorModal renders a replacement modal for S3 website setup failures.
func S3WebsiteInstallErrorModal(message string) ([]byte, error) {
	payload := map[string]any{
		blockKitFieldType:  blockKitTypeModal,
		blockKitFieldTitle: plainTextObj("Protect S3 Website"),
		blockKitFieldClose: plainTextObj("Close"),
		blockKitFieldBlocks: []any{
			sectionBlock(":warning: " + message),
		},
	}
	return json.Marshal(payload)
}
