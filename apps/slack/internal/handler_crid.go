package internal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/layervai/qurl-go/crid"
)

// cridUsageMessage is separate from getUsageMessage because CRIDs are pasted
// without the `$` sigil and do not enter the channel alias resolver.
const cridUsageMessage = "Usage: `/qurl crid <CRID>` to create a qURL directly from a resource's permanent identifier."

// invalidCRIDMessage names the likely fix for a value that fails the local
// CRID gate (checksum, length, charset): a typo, truncated paste, or
// auto-capitalization — CRIDs are never trimmed or case-folded.
const invalidCRIDMessage = "That isn't a valid CRID. Check for a typo, a truncated paste, or capital letters — CRIDs use only lowercase letters and the digits 2–7, with no spaces or `$` prefix."

// cridNotInChannelMessage mirrors [noResourceForAliasMessage]: a CRID that is
// unknown and one that is protected only in another channel get the same copy,
// so the reply never discloses where else a resource is exposed.
const cridNotInChannelMessage = "That CRID is not configured for this channel. Ask your Slack admin to make its resource available here."

// handleCRID implements `/qurl crid <CRID>`. It is `/qurl get` addressed by a
// resource's permanent CRID instead of a channel `$id`/`$alias`: the CRID is
// authorized against the same channel allow-set, then minted through the same
// pipeline ([Handler.mintForResource]).
func (h *Handler) handleCRID(w http.ResponseWriter, values url.Values) {
	cmd, err := Parse(values.Get(fieldText))
	if err != nil {
		switch {
		case errors.Is(err, ErrEmptyCRID):
			respondSlack(w, ":warning: "+cridUsageMessage)
			return
		case errors.Is(err, ErrInvalidCRID):
			respondSlack(w, ":warning: "+invalidCRIDMessage)
			return
		}
		respondSlack(w, ":warning: "+err.Error()+"\n"+cridUsageMessage)
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
	resourceID, keyCandidates := resourceIDForCRID(allowed, args.cmd.CRID)
	if resourceID == "" {
		// A CRID is a permanent, global identifier, so a miss here means the
		// caller holds an identifier for a resource not exposed in this
		// channel — worth an operator-visible line, unlike an alias typo.
		// allow_set_size vs key_candidates separates an empty channel, a
		// real miss, and resource_id encoding drift (candidates == 0).
		// Hash the attempt for correlation without logging the supplied identifier.
		attempt := sha256.Sum256([]byte(args.cmd.CRID))
		log.Warn("crid: CRID not in channel allow-set", "team_id", sanitizeLogValue(args.teamID), "channel_id", sanitizeLogValue(args.channelID), "user_id", sanitizeLogValue(args.userID), "crid_fingerprint", hex.EncodeToString(attempt[:8]), "allow_set_size", len(allowed), "key_candidates", keyCandidates)
		return getResult{}, &userError{msg: cridNotInChannelMessage}
	}
	return h.mintForResource(ctx, log, args, resourceID)
}

// resourceIDForCRID finds the allow-set entry whose public key the CRID
// fingerprints. A resource_id is the base64url DER public key and a CRID is a
// digest of that DER, so the match is local and needs no CRID→resource_id
// lookup. keyCandidates is a complete count only on a miss.
// Any other entry (a legacy id or URL) either fails to decode or
// decodes to bytes whose digest cannot match.
//
// TODO(upstream-contract): mirrors qurl-service's unpadded base64url
// resource_id encoding (same decode as shared/client validateSharingState).
func resourceIDForCRID(allowed map[string]struct{}, cridValue string) (resourceID string, keyCandidates int) {
	for id := range allowed {
		der, err := base64.RawURLEncoding.Strict().DecodeString(id)
		if err != nil || base64.RawURLEncoding.EncodeToString(der) != id {
			continue
		}
		keyCandidates++
		// The CRID already passed crid.Validate at parse time, so an error
		// here is impossible by invariant; treating it as a miss fails closed.
		if matched, err := crid.KeyMatches(cridValue, der); err == nil && matched {
			return id, keyCandidates
		}
	}
	return "", keyCandidates
}
