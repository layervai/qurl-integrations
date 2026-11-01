package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const maxBatchInputBytes = 1 << 20

func batchCmd(opts *globalOpts) *cobra.Command {
	command := &cobra.Command{Use: "batch", Short: "Create up to 100 URL links in one account request", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	command.PersistentFlags().BoolVar(&opts.accountAPIKeyAuth, "api-key-auth", false, "Use an explicit durable account key from QURL_API_KEY_FILE or QURL_API_KEY")
	var filename string
	create := &cobra.Command{Use: "create", Short: "Read a JSON batch and print every indexed result", Args: noArgs, Long: "Read {\"items\":[...]} from stdin or --file. Each item uses the public URL-create API fields. Management uses fresh browser sign-in or explicit --api-key-auth; device credentials are not accepted by the batch API. The command makes one HTTP attempt, never automatically splits or retries, and exits with failure if any item fails. JSON output retains both successful and failed results. Quiet output includes only successful links; errors stay on stderr. Keep stdout secure. An uncertain result can have created links; inspect the account before retrying.", RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := readBatchInput(opts.streams.In, filename)
		if err != nil {
			return err
		}
		if len(raw) > maxBatchInputBytes {
			return exitcode.UsageError(errors.New("batch input exceeds 1 MiB"))
		}
		input, err := qurlapi.ParseBatchCreateInput(raw)
		if err != nil {
			return exitcode.UsageError(err)
		}
		client, _, err := opts.accountManagementClient(cmd.Context(), true)
		if err != nil {
			return err
		}
		result, err := client.BatchCreate(cmd.Context(), input)
		if err != nil {
			return batchWriteFailure(err)
		}
		if err = opts.printer().BatchCreated(result); err != nil {
			return batchWriteFailure(err)
		}
		if result.Failed > 0 {
			return fmt.Errorf("batch created %d links; %d items failed. Inspect indexed results before retrying failed items; keep successful links", result.Succeeded, result.Failed)
		}
		return nil
	}}
	create.Flags().StringVar(&filename, "file", "-", "JSON input file; - reads stdin")
	command.AddCommand(create)
	return command
}

type batchResultError struct{ cause error }

func (e *batchResultError) Error() string { return e.UserMessage() }
func (e *batchResultError) Unwrap() error { return e.cause }
func (e *batchResultError) UserMessage() string {
	return "Batch outcome or output is uncertain. Some links may exist. Do not repeat the whole batch automatically. Inspect the account's resources and links before retrying."
}
func batchWriteFailure(cause error) error {
	var apiError *qurlapi.Error
	if errors.As(cause, &apiError) && apiError.StatusCode >= 400 && apiError.StatusCode < 500 {
		return cause
	}
	return &batchResultError{cause: cause}
}

func readBatchInput(stdin io.Reader, filename string) ([]byte, error) {
	if filename == "-" {
		return io.ReadAll(io.LimitReader(stdin, maxBatchInputBytes+1))
	}
	file, err := os.Open(filename) // #nosec G304 -- The user explicitly selects this local input file.
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxBatchInputBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	return raw, closeErr
}
