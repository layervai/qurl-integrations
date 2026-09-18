package internal

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/layervai/qurl-go/crid"
)

// cridUsageMessage is separate from getUsageMessage because CRIDs are pasted
// without the `$` sigil and do not enter the channel alias resolver.
const cridUsageMessage = "Usage: `/qurl crid <CRID>` to create a qURL directly from a resource's permanent identifier."

// cridNotInChannelMessage mirrors [noResourceForAliasMessage]: a CRID that is
// unknown and one that is protected only in another channel get the same copy,
// so the reply never discloses where else a resource is exposed.
const cridNotInChannelMessage = "That CRID is not configured for this channel. Run `/qurl list` to see what's available here, or contact your Slack admin to add it."

// handleCRID implements `/qurl crid <CRID>`. It is `/qurl get` addressed by a
// resource's permanent CRID instead of a channel `$id`/`$alias`: the CRID is
// authorized against the same channel allow-set, then minted through the same
// pipeline ([Handler.mintForResource]).
func (h *Handler) handleCRID(w http.ResponseWriter, values url.Values) {
	cmd, err := Parse(values.Get(fieldText))
	if err != nil {
		if errors.Is(err, ErrInvalidCRID) {
			respondSlack(w, ":warning: "+cridUsageMessage)
			return
		}
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	h.runAsync(w, string(SubcmdCRID), values, func(ctx context.Context, log *slog.Logger) {
		h.processGet(ctx, log, values, cmd)
	})
}

// cridWork resolves the CRID to a channel-authorized resource_id and mints it.
// An unauthorized CRID returns before mintForResource, so it never burns the
// user's mint quota.
func (h *Handler) cridWork(ctx context.Context, log *slog.Logger, args *getWorkArgs) (getResult, error) {
	allowed, err := h.allowedResourceIDsForGet(ctx, log, args.teamID, args.channelID)
	if err != nil {
		return getResult{}, err
	}
	resourceID, ok := resourceIDForCRID(allowed, args.cmd.CRID)
	if !ok {
		return getResult{}, &userError{msg: cridNotInChannelMessage}
	}
	return h.mintForResource(ctx, log, args, resourceID)
}

// resourceIDForCRID finds the allow-set entry whose public key the CRID
// fingerprints. A resource_id is the base64url DER public key and a CRID is a
// digest of that DER, so the match is local and needs no CRID→resource_id
// lookup. Entries that are not base64url DER (legacy ids) cannot match.
func resourceIDForCRID(allowed map[string]struct{}, cridValue string) (string, bool) {
	for id := range allowed {
		der, err := base64.RawURLEncoding.Strict().DecodeString(id)
		if err != nil {
			continue
		}
		if matched, err := crid.KeyMatches(cridValue, der); err == nil && matched {
			return id, true
		}
	}
	return "", false
}
