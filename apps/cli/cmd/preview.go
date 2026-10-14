package main

import (
	"errors"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const (
	msgPreviewDescriptionRequired = "provide --name or --description; use an empty string to remove it"
	msgPreviewAltRequired         = "provide --alt to describe the image or clip"
	msgPreviewUploaded            = "File uploaded. Processing keeps current media visible. Check with qurl preview get %s."
	msgPreviewMediaMissing        = "media ID not found; read qurl preview get first"
)

func previewCmd(opts *globalOpts) *cobra.Command {
	root := &cobra.Command{Use: "preview", Short: "Show or edit one CRID's public preview metadata"}
	get := &cobra.Command{Use: "get <CRID>", Short: "Show description, media and processing state", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := opts.newClient(cmd.Context())
		if err != nil {
			return err
		}
		preview, err := client.Preview(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return opts.printer().Preview(preview)
	}}
	set := previewSetCmd(opts)
	var alt, replaceID string
	upload := &cobra.Command{Use: "upload <CRID> <file>", Short: "Upload an image, GIF or short clip (verified publishers only)", Args: exactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if alt == "" {
			return exitcode.UsageError(errors.New(msgPreviewAltRequired))
		}
		client, err := opts.newClient(cmd.Context())
		if err != nil {
			return err
		}
		next, err := client.UploadPreview(cmd.Context(), args[0], args[1], alt, replaceID)
		if err != nil {
			var invalid qurlapi.PreviewFileError
			if errors.As(err, &invalid) {
				return exitcode.InvalidInputError(invalid.Error(), err)
			}
			return err
		}
		if !opts.quiet && opts.resolvedFormat != output.FormatJSON {
			opts.printer().Notef(msgPreviewUploaded, args[0])
		}
		return opts.printer().Preview(next)
	}}
	upload.Flags().StringVar(&alt, "alt", "", "description for people who cannot see the image or clip")
	upload.Flags().StringVar(&replaceID, "replace", "", "media ID to replace after processing succeeds")
	remove := previewRemoveCmd(opts)
	clearCmd := &cobra.Command{Use: "clear <CRID>", Short: "Remove name, description and media and cancel pending uploads", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := opts.newClient(cmd.Context())
		if err != nil {
			return err
		}
		current, err := client.Preview(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		text := ""
		ids := []string{}
		next, err := client.UpdatePreview(cmd.Context(), args[0], qurlapi.PreviewPatch{Revision: current.Revision, Name: &text, Description: &text, MediaIDs: &ids})
		if err != nil {
			return err
		}
		return opts.printer().Preview(next)
	}}
	root.AddCommand(get, set, upload, remove, clearCmd)
	return root
}

func previewRemoveCmd(opts *globalOpts) *cobra.Command {
	remove := &cobra.Command{Use: "remove <CRID> <media-id>", Short: "Remove one media item", Args: exactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := opts.newClient(cmd.Context())
		if err != nil {
			return err
		}
		current, err := client.Preview(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		ids := []string{}
		found := false
		for i := range current.Media {
			media := &current.Media[i]
			if media.ID == args[1] {
				found = true
			} else {
				ids = append(ids, media.ID)
			}
		}
		if !found {
			return exitcode.UsageError(errors.New(msgPreviewMediaMissing))
		}
		next, err := client.UpdatePreview(cmd.Context(), args[0], qurlapi.PreviewPatch{Revision: current.Revision, MediaIDs: &ids})
		if err != nil {
			return err
		}
		return opts.printer().Preview(next)
	}}
	return remove
}

func previewSetCmd(opts *globalOpts) *cobra.Command {
	var name, description string
	set := &cobra.Command{Use: "set <CRID>", Short: "Set a public name (up to 100 characters) or description (up to 500 characters)", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("name") && !cmd.Flags().Changed("description") {
			return exitcode.UsageError(errors.New(msgPreviewDescriptionRequired))
		}
		client, err := opts.newClient(cmd.Context())
		if err != nil {
			return err
		}
		current, err := client.Preview(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		patch := qurlapi.PreviewPatch{Revision: current.Revision}
		if cmd.Flags().Changed("name") {
			patch.Name = &name
		}
		if cmd.Flags().Changed("description") {
			patch.Description = &description
		}
		next, err := client.UpdatePreview(cmd.Context(), args[0], patch)
		if err != nil {
			return err
		}
		return opts.printer().Preview(next)
	}}
	set.Flags().StringVar(&name, "name", "", "public app name; empty removes it")
	set.Flags().StringVar(&description, "description", "", "public preview text; empty removes it")
	return set
}
