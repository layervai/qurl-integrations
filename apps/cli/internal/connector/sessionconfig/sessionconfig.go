// Package sessionconfig binds native qURL Connector session operations to the
// authenticated account. Deployment authority stays private to the NHP server.
package sessionconfig

import (
	"errors"
	"fmt"
	"strings"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
)

// ErrConfig identifies a missing or invalid authenticated owner binding.
var ErrConfig = errors.New("qURL Connector native session configuration")

// Resolve builds the complete public session-operation authority. AWS account,
// region, and table names are private NHP runtime configuration and never enter
// the Connector binary or its state.
func Resolve(ownerID string) (connectorshare.NativeSessionOperationAuthority, error) {
	authority := connectorshare.NativeSessionOperationAuthority{OwnerID: strings.TrimSpace(ownerID)}
	if err := connectorshare.ValidateNativeSessionOperationAuthority(authority); err != nil {
		return connectorshare.NativeSessionOperationAuthority{}, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	return authority, nil
}
