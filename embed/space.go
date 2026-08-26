package embed

import (
	"errors"
	"fmt"
)

// Space identifies an embedding space on its own, for vectors that were
// produced somewhere ragit cannot call — an extraction service that embeds as
// a side effect of extracting, say, or a batch job that ran last week.
//
// It exists so the identity of those vectors is *declared* rather than
// formatted by hand. Retrieval filters on the fingerprint, so a corpus written
// under a string that disagrees by one character with what the query embedder
// reports returns nothing at all — indistinguishable from an empty corpus, and
// silent. A struct with three named fields cannot drift that way.
type Space struct {
	// Provider identifies the backend that produced the vectors, e.g.
	// "xberg". It is part of the identity: the same model served by two
	// providers is not reliably the same space.
	Provider string
	// Model is the embedding model id, e.g. "bge-base-en-v1.5".
	Model string
	// Dimension is the vector width, and must match the width the schema was
	// generated for.
	Dimension int
}

// SpaceOf is the space an [Embedder] embeds into.
func SpaceOf(e Embedder) Space {
	return Space{Provider: e.Provider(), Model: e.Model(), Dimension: e.Dimension()}
}

// Fingerprint is the canonical identity of the space: provider|model|dimension.
func (s Space) Fingerprint() string {
	return fmt.Sprintf("%s|%s|%d", s.Provider, s.Model, s.Dimension)
}

// Validate rejects a space that could not identify anything.
//
// A fingerprint with an empty field still compares equal to itself, so an
// unset Space would work perfectly until a second one showed up — which is
// exactly when it would matter.
func (s Space) Validate() error {
	switch {
	case s.Provider == "":
		return errors.New("embed: Space.Provider is required")
	case s.Model == "":
		return errors.New("embed: Space.Model is required")
	case s.Dimension <= 0:
		return fmt.Errorf("embed: Space.Dimension must be positive, got %d", s.Dimension)
	}
	return nil
}

// Fingerprint is the canonical identity of the space an embedder embeds into.
// Two embedders with the same fingerprint produce comparable vectors;
// different fingerprints do not, even if they happen to share a dimension.
func Fingerprint(e Embedder) string { return SpaceOf(e).Fingerprint() }
