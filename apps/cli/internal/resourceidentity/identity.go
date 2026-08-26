// Package resourceidentity validates public resource keys and their CRIDs.
package resourceidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/layervai/qurl-go/crid"
)

// ValidateResourceID decodes and validates a canonical public resource key.
func ValidateResourceID(value string) ([]byte, error) {
	der, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(der) != value {
		return nil, errors.New("resource identity is not canonical unpadded base64url")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("resource identity is not a DER SPKI public key")
	}
	publicKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return nil, errors.New("resource identity is not a P-256 public key")
	}
	canonical, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil || !bytes.Equal(canonical, der) {
		return nil, errors.New("resource identity DER is not canonical")
	}
	return der, nil
}

// ValidatePair requires cridValue to be a valid CRID for resourceID.
func ValidatePair(cridValue, resourceID string) error {
	der, err := ValidateResourceID(resourceID)
	if err != nil {
		return err
	}
	matched, err := crid.KeyMatches(cridValue, der)
	if err != nil {
		return fmt.Errorf("CRID is invalid: %w", err)
	}
	if !matched {
		return errors.New("CRID does not match resource identity")
	}
	return nil
}
