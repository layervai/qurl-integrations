package internal

import (
	"context"
	"errors"

	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	channelTokenIncompleteMessage = "Couldn't confirm a unique resource from the incomplete list. Ask your Slack admin to bind an alias in this channel, then use that alias."
	channelTokenAlias             = "channel alias"
	channelTokenSlug              = "connector slug"
	channelTokenResourceAlias     = "resource alias"
)

var (
	errChannelTokenIncomplete = errors.New("channel token candidate set is incomplete")
	errChannelTokenNotFound   = errors.New("channel token not found")
	errChannelTokenLegacy     = errors.New("legacy direct URL binding")
	errChannelTokenAmbiguous  = errors.New("ambiguous channel token")
)

type channelTokenResource struct {
	ResourceID string
	Resource   *client.Resource
	Via        string
}

// Sources may use live queries or a channel-filtered snapshot. Alias bindings
// authorize by themselves; their metadata may be absent from a stale snapshot.
type channelTokenSource struct {
	alias      func() (*channelTokenResource, error)
	allowed    func() (map[string]struct{}, error)
	candidates func(string, map[string]struct{}) ([]client.Resource, error)
}

func resolveChannelToken(token string, source channelTokenSource) (*channelTokenResource, error) {
	bound, err := source.alias()
	if err != nil && !errors.Is(err, errChannelTokenNotFound) {
		return nil, err
	}
	if bound != nil {
		if isLegacyDirectURLBinding(bound.ResourceID) {
			return nil, errChannelTokenLegacy
		}
		bound.Via = channelTokenAlias
		return bound, nil
	}
	allowed, err := source.allowed()
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		return nil, errChannelTokenNotFound
	}
	for _, via := range []string{channelTokenSlug, channelTokenResourceAlias} {
		resources, err := source.candidates(via, allowed)
		if err != nil {
			return nil, err
		}
		var match *client.Resource
		for i := range resources {
			r := &resources[i]
			if _, ok := allowed[r.ResourceID]; !ok {
				continue
			}
			matches := r.Type == client.ResourceTypeTunnel && r.Status == client.StatusActive && r.Slug == token
			if via == channelTokenResourceAlias {
				matches = isURLResource(r) && r.Status != client.StatusRevoked && r.Alias == token
			}
			if !matches {
				continue
			}
			if match != nil && match.ResourceID != r.ResourceID {
				return nil, errChannelTokenAmbiguous
			}
			match = r
		}
		if match != nil {
			return &channelTokenResource{ResourceID: match.ResourceID, Resource: match, Via: via}, nil
		}
	}
	return nil, errChannelTokenNotFound
}

// A bounded alias lookup cannot prove uniqueness if unseen allowed IDs remain.
func lookupChannelResourceAliasCandidates(ctx context.Context, c *client.Client, allowed map[string]struct{}) ([]client.Resource, error) {
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		return nil, err
	}
	if page.HasMore {
		missing := make(map[string]struct{}, len(allowed))
		for id := range allowed {
			missing[id] = struct{}{}
		}
		for i := range page.Resources {
			delete(missing, page.Resources[i].ResourceID)
		}
		if len(missing) != 0 {
			return nil, errChannelTokenIncomplete
		}
	}
	return page.Resources, nil
}
