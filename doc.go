// Package jsoncap provides token-level bounded decoding of untrusted
// upstream JSON with json.Unmarshal-parity semantics.
//
// json.Unmarshal materializes the entire decoded value before any
// caller-side count check can run, so compact serialized elements ("[0,0,0,"
// repeated) amplify a wire-capped body into decoded structs and slice
// backing arrays far beyond the byte cap. A Decoder instead walks the token
// stream and lets the caller enforce every cardinality cap BEFORE an element
// is decoded — a per-array cap and an aggregate element budget both reject
// hostile cardinality before allocation scales with it.
//
// The building blocks reproduce encoding/json's observable semantics
// exactly, so a schema decoder built from them is a drop-in for
// json.Unmarshal on well-formed input:
//
//   - a JSON null where a container is expected is a no-op for objects
//     (Object leaves the target untouched) and sets the slice nil for
//     arrays (Array), matching Unmarshal's null handling; an empty array
//     allocates an empty non-nil slice, also matching Unmarshal;
//   - duplicate object keys merge field-wise (decode into the existing
//     value), and duplicate array keys re-expose retained backing within
//     capacity, truncate to the new length, and replace the slice on an
//     empty re-occurrence — Array owns that lifecycle;
//   - a JSON object decoded into a Go MAP (Map) charges its entries against
//     the same aggregate budget the array walk uses, and reproduces
//     Unmarshal's map semantics, where a null yields nil (as for a slice, not
//     as for a struct) and a duplicate key REPLACES with a fresh zero value
//     rather than merging field-wise;
//   - unknown fields are token-skipped without materializing (Skip);
//   - scalar values decode via json.Decoder.Decode for stdlib-identical
//     type handling (Decode).
//
// Key dispatch stays caller-side; match keys with strings.EqualFold to
// reproduce json.Unmarshal's case-insensitive field fallback.
//
// The underlying json.Decoder runs with UseNumber, so skipping an unknown
// field never converts its numbers through float64 (which would reject
// syntactically valid values like 1e1000 that json.Unmarshal's field
// skipping accepts). Decoding into typed int/string/bool fields is
// unaffected; a caller decoding into an untyped any receives json.Number.
//
// Preflight is the other half of the same concern: where the Decoder bounds
// what a schema decode ALLOCATES, Preflight is a standalone pass that rejects
// a body whose STRUCTURE is ambiguous before any decode runs — a repeated
// object key, which json.Unmarshal resolves to the last occurrence, destroying
// the evidence that it happened. It is the fail-closed counterpart to Object
// and Array, which deliberately REPRODUCE Unmarshal's duplicate-key merge: a
// schema decoder must match the stdlib, while a caller that cannot tolerate
// the ambiguity rejects the body outright. Nesting depth needs no pass of its
// own — encoding/json bounds it at MaxDepth for every walk here, Preflight
// included.
package jsoncap
