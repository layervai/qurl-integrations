package matchedcohort

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// RegistrySchema is the only accepted fixed-canary registry schema.
const RegistrySchema = 1

// GenerationPointer binds one generation to its immutable authority version.
type GenerationPointer struct {
	GenerationID string         `json:"generation_id"`
	Authority    StateReference `json:"authority"`
}

// Registry is the small active-generation CAS. Activation never deletes the
// prior generation. Explicit rotation cleanup remains blocked until its exact
// remote revoke receipts are durable; normal releases may only read Active.
type Registry struct {
	Schema              int                 `json:"schema"`
	Environment         string              `json:"environment"`
	Active              GenerationPointer   `json:"active"`
	RetainedGenerations []GenerationPointer `json:"retained_generations"`
}

// Rotator atomically activates a complete generation while retaining the old one.
type Rotator struct {
	Blobs           BlobAuthority
	WriterLock      CredentialWriterLock
	RegistryKey     string
	InvocationToken string
}

// Activate moves the registry pointer only after exact authority readback.
func (r *Rotator) Activate(ctx context.Context, generation ProvisionedGeneration) (Registry, StateReference, error) { //nolint:gocritic // Immutable generation value and explicit classifications are intentional.
	if r == nil || r.Blobs == nil || r.WriterLock == nil || !validText(r.RegistryKey) || !hex64Pattern.MatchString(r.InvocationToken) || ValidateAuthority(generation.Authority) != nil {
		return Registry{}, StateReference{}, fmt.Errorf("%w: rotation input", errInvalidAuthority)
	}
	authorityRaw, err := CanonicalJSON(generation.Authority)
	if err != nil {
		return Registry{}, StateReference{}, err
	}
	var registry Registry
	var receipt StateReference
	lockOperation := CredentialWriterOperation{Schema: 1, OwnerSubject: generation.Authority.OwnerSubject, Operation: "rotate",
		GenerationID: generation.Authority.GenerationID, PlanSHA256: Digest(authorityRaw), InvocationToken: r.InvocationToken}
	if err := r.WriterLock.WithExclusive(ctx, lockOperation, func(lockedCtx context.Context) error {
		var activateErr error
		registry, receipt, activateErr = r.activateUnlocked(lockedCtx, generation)
		return activateErr
	}); err != nil {
		return Registry{}, StateReference{}, err
	}
	return registry, receipt, nil
}

func (r *Rotator) activateUnlocked(ctx context.Context, generation ProvisionedGeneration) (Registry, StateReference, error) { //nolint:gocritic // Caller holds the durable credential writer.
	if generation.Reference.Key != fmt.Sprintf("generations/%s/authority", generation.Authority.GenerationID) {
		return Registry{}, StateReference{}, fmt.Errorf("%w: authority reference key", errInvalidAuthority)
	}
	loaded, err := r.Blobs.Load(ctx, generation.Reference.Key)
	if err != nil || loaded.VersionID != generation.Reference.VersionID || loaded.SHA256 != generation.Reference.SHA256 {
		return Registry{}, StateReference{}, fmt.Errorf("%w: authority reference readback", errStateConflict)
	}
	var exact Authority
	if err := json.Unmarshal(loaded.Body, &exact); err != nil || ValidateAuthority(exact) != nil || exact.GenerationID != generation.Authority.GenerationID {
		return Registry{}, StateReference{}, fmt.Errorf("%w: authority readback", errStateConflict)
	}

	registry, previous, err := r.loadRegistry(ctx)
	if err != nil {
		return Registry{}, StateReference{}, err
	}
	next := GenerationPointer{GenerationID: exact.GenerationID, Authority: generation.Reference}
	if registry.Active == next {
		return registry, blobReference(previous), nil
	}
	if registry.Active.GenerationID != "" {
		if registry.Active.GenerationID == next.GenerationID || containsGeneration(registry.RetainedGenerations, next.GenerationID) {
			return Registry{}, StateReference{}, fmt.Errorf("%w: generation pointer drift", errStateConflict)
		}
		registry.RetainedGenerations = append(registry.RetainedGenerations, registry.Active)
	}
	registry.Active = next
	raw, err := CanonicalJSON(registry)
	if err != nil {
		return Registry{}, StateReference{}, err
	}
	digest := Digest(raw)
	operationID := Digest([]byte("layerv/matched-cohort-registry/v1\x00" + r.RegistryKey + "\x00" + previous.VersionID + "\x00" + digest))
	candidate := BlobCandidate{Key: r.RegistryKey, ExpectedVersion: previous.VersionID, OperationID: operationID, SHA256: digest, Body: raw}
	committed, err := r.Blobs.Commit(ctx, candidate)
	if err != nil {
		observed, loadErr := r.Blobs.Load(ctx, r.RegistryKey)
		if loadErr != nil || !sameCommittedBlob(observed, candidate) {
			return Registry{}, StateReference{}, fmt.Errorf("%w: activate generation", errStateAmbiguous)
		}
		committed = observed
	}
	if !sameCommittedBlob(committed, candidate) {
		return Registry{}, StateReference{}, fmt.Errorf("%w: activation receipt", errStateConflict)
	}
	return registry, blobReference(committed), nil
}

func (r *Rotator) loadRegistry(ctx context.Context) (Registry, Blob, error) {
	blob, err := r.Blobs.Load(ctx, r.RegistryKey)
	if errors.Is(err, errStateNotFound) {
		return Registry{Schema: RegistrySchema, Environment: EnvironmentSandbox, RetainedGenerations: []GenerationPointer{}}, Blob{Key: r.RegistryKey}, nil
	}
	if err != nil {
		return Registry{}, Blob{}, err
	}
	if Digest(blob.Body) != blob.SHA256 || !validText(blob.VersionID) {
		return Registry{}, Blob{}, fmt.Errorf("%w: registry receipt", errStateConflict)
	}
	var registry Registry
	if err := json.Unmarshal(blob.Body, &registry); err != nil || registry.Schema != RegistrySchema || registry.Environment != EnvironmentSandbox {
		return Registry{}, Blob{}, fmt.Errorf("%w: registry JSON", errStateConflict)
	}
	canonical, _ := CanonicalJSON(registry)
	if !bytes.Equal(canonical, blob.Body) || duplicateGenerations(registry) {
		return Registry{}, Blob{}, fmt.Errorf("%w: registry binding", errStateConflict)
	}
	return registry, cloneBlob(blob), nil
}

func duplicateGenerations(registry Registry) bool { //nolint:gocritic // Registry is a small closed value.
	seen := map[string]struct{}{}
	for _, pointer := range append([]GenerationPointer{registry.Active}, registry.RetainedGenerations...) {
		if pointer.GenerationID == "" {
			continue
		}
		if _, exists := seen[pointer.GenerationID]; exists {
			return true
		}
		seen[pointer.GenerationID] = struct{}{}
	}
	return false
}

func containsGeneration(values []GenerationPointer, generationID string) bool {
	return slices.ContainsFunc(values, func(value GenerationPointer) bool { return value.GenerationID == generationID })
}
