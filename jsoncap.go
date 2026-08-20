package jsoncap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sentinel errors, matched with errors.Is through the wrapped errors the
// budget and cap checks return.
var (
	// ErrElementBudget reports that the Decoder's aggregate element budget
	// was exhausted: the total number of array elements and map entries
	// decoded (across every Array and Map call on this Decoder) exceeded the
	// budget given to NewDecoder.
	ErrElementBudget = errors.New("jsoncap: element budget exceeded")
	// ErrArrayCap reports that one array exceeded its per-Array cardinality
	// cap. The wrapping error names the array via Array's what argument.
	ErrArrayCap = errors.New("jsoncap: array cardinality cap exceeded")
	// ErrMapCap reports that one map exceeded its per-Map cardinality cap.
	// The wrapping error names the map via Map's what argument, and never a
	// key: at this boundary the keys are attacker-shaped text.
	ErrMapCap = errors.New("jsoncap: map cardinality cap exceeded")
	// ErrDuplicateKey reports that Preflight found an object repeating a key
	// (matched case-insensitively). The wrapping error carries a bounded,
	// quoted snippet of the offending key.
	ErrDuplicateKey = errors.New("jsoncap: duplicate object key")
)

// MaxDepth is the nesting ceiling every walk in this package observes: at most
// MaxDepth containers open at once. encoding/json enforces it, not this
// package - the constant exists because the stdlib does not export the value,
// so without it a caller has no name for the threshold and no way to write the
// boundary body a test needs.
//
// Since Go 1.27 encoding/json is backed by encoding/json/v2, whose jsontext
// Decoder refuses to read the token that would open container MaxDepth+1. That
// refusal reaches every entry point here, because every one of them reads
// tokens: Preflight, Skip, Object, Array, Map, Decode and a hand-rolled
// Open/More/Close loop are all bounded by it. It arrives as encoding/json's own
// *json.SyntaxError; this package does not translate it, because there is
// nothing stable to translate it to (jsontext keeps its depth error unexported
// and its exported SyntacticError documents its contents as subject to change,
// so the only hook is the message text - and classifying an error by its text
// is exactly what a sentinel exists to avoid).
//
// Measured on Go 1.27.0, for both nested arrays and nested objects: Preflight
// and json.Unmarshal accept MaxDepth and reject MaxDepth+1 with the identical
// *json.SyntaxError. So Preflight's depth acceptance set is EXACTLY
// json.Unmarshal's, which is the parity Preflight promises, and it can never
// admit a body the decode step would reject on depth. That parity is pinned by
// TestPreflightDepthMatchesUnmarshal, which asserts both boundary levels
// against json.Unmarshal itself rather than against this constant - so if a
// future toolchain moves the stdlib's ceiling, the test fails instead of the
// constant silently drifting.
//
// Historical note, because it explains a mechanism that is no longer here: on
// Go 1.26 and earlier json.Decoder.Token applied NO depth limit, so a token
// walk over a 1 MiB body of '[' recursed once per byte (~1M frames, measured at
// 206 MB RSS inside a 256 MiB container). Preflight carried its own explicit
// depth check to compensate, returning a local sentinel. Go 1.27 provides that
// bound itself, at this exact threshold, so the check was deleted rather than
// re-tuned: keeping it meant either duplicating a stdlib guarantee or - by
// lowering the ceiling so the local check could still fire first - rejecting
// depth-MaxDepth bodies that json.Unmarshal accepts, which would have traded a
// true parity claim for a preserved error name. Measured with the check gone: a
// 1 MiB all-opens body is still refused, and stack use stays flat at 2688 KiB.
const MaxDepth = 10000

// maxKeySnippet bounds the untrusted key text a duplicate-key error renders,
// so an oversized upstream key cannot balloon an error string or a log line.
// The key is rendered with %q, which escapes controls and newlines, so the
// message stays single-line and safe to log as-is; a caller with a stricter
// text policy applies it to its own wrapping message.
const maxKeySnippet = 64

// Decoder walks one JSON value as a token stream, charging every decoded
// array element and map entry against an aggregate budget. It is not safe for
// concurrent use.
type Decoder struct {
	dec      *json.Decoder
	limit    int
	elements int
}

// NewDecoder returns a Decoder reading from r with the given aggregate
// element budget. Construction through NewDecoder is mandatory: the zero
// Decoder has no underlying json.Decoder, so every decoding method
// nil-dereferences. Every array element decoded through Array and every map
// entry decoded through Map is charged against the budget, whichever
// container it belongs to, so deeply nested or repeated containers cannot
// multiply a per-container cap. elementBudget <= 0 means no aggregate budget
// (per-container caps still apply).
func NewDecoder(r io.Reader, elementBudget int) *Decoder {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return &Decoder{dec: dec, limit: elementBudget}
}

// Elements reports how many array elements and map entries have been charged
// against the budget so far, so a caller paginating across multiple bodies can
// carry one budget across Decoders.
func (d *Decoder) Elements() int { return d.elements }

// More reports whether the current array or object has another element,
// delegating to json.Decoder.More.
func (d *Decoder) More() bool { return d.dec.More() }

// Decode decodes the next value via json.Decoder.Decode, giving
// stdlib-identical handling for scalar and leaf values.
func (d *Decoder) Decode(v any) error { return d.dec.Decode(v) }

// count charges one decoded array element or map entry against the aggregate
// budget.
func (d *Decoder) count() error {
	d.elements++
	if d.limit > 0 && d.elements > d.limit {
		return fmt.Errorf("%w: %d", ErrElementBudget, d.limit)
	}
	return nil
}

// Open consumes the opening delimiter of a container. It reports ok=false
// (without error) for a JSON null, so the caller can implement Unmarshal's
// null-into-value semantics, and errors on any other token.
func (d *Decoder) Open(delim json.Delim) (ok bool, err error) {
	t, err := d.dec.Token()
	if err != nil {
		return false, err
	}
	if t == nil {
		return false, nil
	}
	if got, isDelim := t.(json.Delim); !isDelim || got != delim {
		return false, fmt.Errorf("expected %q, got %v", delim, t)
	}
	return true, nil
}

// Close consumes a container's closing delimiter (the token json.Decoder
// guarantees once More reports false).
func (d *Decoder) Close() error {
	_, err := d.dec.Token()
	return err
}

// Key reads the next object key.
func (d *Decoder) Key() (string, error) {
	t, err := d.dec.Token()
	if err != nil {
		return "", err
	}
	s, isString := t.(string)
	if !isString {
		return "", fmt.Errorf("expected object key, got %v", t)
	}
	return s, nil
}

// Skip consumes and discards one whole value (scalar or container) without
// materializing it — json.Unmarshal's unknown-field handling without the
// allocation.
func (d *Decoder) Skip() error {
	depth := 0
	for {
		t, err := d.dec.Token()
		if err != nil {
			return err
		}
		if delim, isDelim := t.(json.Delim); isDelim {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// Object decodes one JSON object by walking its keys: field is invoked once
// per key and must consume exactly that key's value (Decode a known field,
// Skip an unknown one, or recurse into Object/Array for a container). A
// JSON null in place of the object is a no-op — field is never invoked and
// the caller's target stays untouched — matching json.Unmarshal's
// null-into-struct semantics, which is what makes a duplicate key like
// `"expand": null` unable to wipe an already-decoded value. Dispatch keys
// with strings.EqualFold for Unmarshal's case-insensitive field matching.
func (d *Decoder) Object(field func(key string) error) error {
	ok, err := d.Open('{')
	if err != nil || !ok {
		return err
	}
	for d.dec.More() {
		k, err := d.Key()
		if err != nil {
			return err
		}
		if err := field(k); err != nil {
			return err
		}
	}
	return d.Close()
}

// End verifies the input is exhausted after the top-level value: anything
// but io.EOF — trailing data or a syntax error — is rejected, matching
// json.Unmarshal's whole-input strictness (json.Decoder alone would leave
// trailing garbage unread and unreported).
func (d *Decoder) End() error {
	if _, err := d.dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("trailing data after top-level value: %w", err)
		}
		return errors.New("trailing data after top-level value")
	}
	return nil
}

// Array decodes one JSON array under the shared bounded lifecycle:
// per-array cap check BEFORE the element is counted (an over-cap array
// errors with ErrArrayCap, named by what), aggregate budget charge BEFORE
// the element is allocated (ErrElementBudget), decode INTO the regrown
// element, and truncation at the decoded length.
//
// prior is the already-decoded value of a previous occurrence of the same
// key, giving json.Unmarshal's duplicate-key slice semantics: elements
// decode into the existing slice (a within-capacity regrow re-exposes the
// retained backing element, so field-wise merge matches stdlib), the result
// truncates to the new array's length, and an empty re-occurrence REPLACES
// the slice (a fresh empty non-nil slice, no retained backing a later
// occurrence could re-expose). A JSON null in place of the array yields nil
// without error, matching Unmarshal's null-into-slice; an empty array `[]`
// yields an empty non-nil slice, matching Unmarshal's empty-array
// allocation. Pass maxElems <= 0 for no per-array cap (the aggregate budget
// still applies).
func Array[T any](d *Decoder, prior []T, maxElems int, what string, decodeElem func(*T) error) ([]T, error) {
	ok, err := d.Open('[')
	if err != nil || !ok {
		return nil, err
	}
	s := prior
	n := 0
	for d.dec.More() {
		if maxElems > 0 && n >= maxElems {
			return nil, fmt.Errorf("%s: %w: %d", what, ErrArrayCap, maxElems)
		}
		if err := d.count(); err != nil {
			return nil, err
		}
		s = growForIndex(s, n)
		if err := decodeElem(&s[n]); err != nil {
			return nil, err
		}
		n++
	}
	return truncateArray(s, n), d.Close()
}

// growForIndex ensures the slice covers index n, matching json.Unmarshal's
// slice-regrow semantics for duplicate keys: within retained capacity the
// existing backing element is re-exposed (stdlib SetLen), beyond capacity a
// zero element is appended (stdlib Grow reallocates; the new tail is zero).
func growForIndex[T any](s []T, n int) []T {
	if n < len(s) {
		return s
	}
	if n < cap(s) {
		return s[:n+1]
	}
	var zero T
	return append(s, zero)
}

// truncateArray finalizes a decoded array at n elements, matching
// json.Unmarshal's end-of-array semantics: an empty array REPLACES the
// slice with a fresh empty non-nil slice (stdlib MakeSlice(0,0) — allocated,
// so `[]` is distinguishable from null's nil, and no retained backing a
// later duplicate occurrence could re-expose), a non-empty one truncates in
// place (stdlib SetLen).
func truncateArray[T any](s []T, n int) []T {
	if n == 0 {
		return []T{}
	}
	return s[:n]
}

// Map decodes one JSON object into a Go map under the same bounded lifecycle
// Array gives a slice: per-map cap check BEFORE the entry costs anything (an
// over-cap map errors with ErrMapCap, named by what), aggregate budget charge
// BEFORE the entry is read (ErrElementBudget), then key, value, store.
// decodeValue is invoked once per entry with that entry's key and a pointer to
// the zero value it must decode into (Decode a scalar, or recurse into Object,
// Array or Map for a container); the key is passed so a caller with a per-key
// policy can apply it without a second walk. Pass maxEntries <= 0 for no
// per-map cap (the aggregate budget still applies).
//
// It is a twin of Array rather than an exported entry-charge primitive, and
// the choice follows the package's shape: every cap and charge here is applied
// BY the walk, in an order a caller cannot get wrong (cap before charge,
// charge before the entry is read), and the walk also owns the Unmarshal
// semantics that go with the container. An exported counter would hand both
// back to every caller - the order to re-derive per walk, the map's
// null/duplicate/allocation semantics to re-derive per schema - which is the
// drift this package was extracted to end. The charge lands before the KEY is
// read because the key is itself an unbounded allocation from the wire, so a
// budget that only bounded values would not bound the decode. A caller whose
// map policy genuinely does not fit this shape keeps the token primitives
// (Open, Key, Decode, Skip, More, Close) and its own accounting, exactly as it
// can for arrays - it simply does not participate in this Decoder's aggregate.
//
// The map semantics reproduce json.Unmarshal's, which for maps differ from
// both of the other walks in ways worth stating:
//
//   - A JSON null in place of the object yields nil, wiping an already-decoded
//     earlier occurrence of the same key. That is Unmarshal's null-into-MAP
//     handling, which follows its null-into-slice (Array) and NOT its
//     null-into-struct (Object, where a null is a no-op) - so a caller reading
//     a nil map as a structural sentinel gets that sentinel from a hostile
//     `"m":null` too, which is the fail-closed direction anyway.
//   - Each entry decodes into a FRESH zero value, so a duplicate KEY replaces
//     rather than merging field-wise (`{"a":{"x":1},"a":{"y":2}}` leaves only
//     y set, where Array's prior would have merged), and a null value stores
//     the zero value.
//   - An empty object `{}` allocates an empty non-nil map when prior is nil,
//     matching Unmarshal's allocation, but does NOT replace a non-nil prior
//     (Array's empty re-occurrence does replace its slice) - Unmarshal only
//     allocates a nil map, and an empty object then adds nothing.
//   - A non-nil prior is decoded INTO, so a duplicate object key merges the
//     two occurrences' entries and keys absent from the second survive.
//   - Keys compare by the map's own exact equality: Unmarshal matches struct
//     FIELDS case-insensitively but map keys case-sensitively, so "a" and "A"
//     are two entries here.
//
// On error the returned map holds the entries decoded so far, exactly as a
// failed json.Unmarshal leaves them in the caller's map; the error, not the
// map, is the result.
func Map[V any](d *Decoder, prior map[string]V, maxEntries int, what string, decodeValue func(key string, v *V) error) (map[string]V, error) {
	ok, err := d.Open('{')
	if err != nil {
		// Nothing was consumed into the map, so the caller's own value stands
		// - Unmarshal leaves a target untouched when the value is the wrong
		// shape for it.
		return prior, err
	}
	if !ok {
		return nil, nil
	}
	m := prior
	if m == nil {
		m = make(map[string]V)
	}
	n := 0
	for d.dec.More() {
		if maxEntries > 0 && n >= maxEntries {
			return m, fmt.Errorf("%s: %w: %d", what, ErrMapCap, maxEntries)
		}
		if err := d.count(); err != nil {
			return m, err
		}
		key, err := d.Key()
		if err != nil {
			return m, err
		}
		var v V
		if err := decodeValue(key, &v); err != nil {
			return m, err
		}
		m[key] = v
		n++
	}
	return m, d.Close()
}

// Preflight walks one complete JSON value from r and rejects the structural
// defect json.Unmarshal silently tolerates. It decodes nothing into a caller
// value.
//
// Duplicate object keys: encoding/json accepts a repeated key and applies the
// LAST occurrence to the struct field, discarding the earlier value unseen. A
// body carrying a real value and then a null for the same key therefore
// decodes as the null, and no schema decoder downstream can tell that
// happened - the evidence is gone before it is reachable. Matching is
// case-insensitive because encoding/json matches struct FIELDS
// case-insensitively too, so "media" and "Media" address the same field and
// are equally ambiguous. The first repeat fails with ErrDuplicateKey. Note
// this is the opposite fail direction from Object and Array, which reproduce
// Unmarshal's duplicate-key MERGE semantics: a schema decoder must behave
// exactly like the stdlib, while a caller that cannot tolerate the ambiguity
// at all rejects the body before decoding it.
//
// Nesting depth is bounded but not by this pass: encoding/json refuses the
// token that would open container MaxDepth+1, so an over-deep body fails with
// its *json.SyntaxError and Preflight's depth acceptance set is exactly
// json.Unmarshal's. See MaxDepth.
//
// It is a PREFLIGHT, not a decode: run it over the whole body, then hand the
// same bytes to json.Unmarshal or a Decoder schema walk. It reads r to
// completion and rejects trailing data after the top-level value (End's
// whole-input strictness), so it never accepts a body the decode step would
// reject. Content policy stays the caller's: Preflight takes no view of which
// keys or values are acceptable, only of whether the structure is unambiguous.
// Invalid UTF-8 is likewise not its concern - json.Unmarshal replaces
// malformed bytes inside strings with U+FFFD rather than failing, and whether
// that is acceptable depends on what the caller does with the decoded text.
func Preflight(r io.Reader) error {
	d := NewDecoder(r, 0)
	if err := d.preflightValue(); err != nil {
		return err
	}
	return d.End()
}

// preflightValue consumes exactly one JSON value, recursing into objects and
// arrays. A scalar is consumed by the leading Token call; a container is
// closed by the trailing one. The recursion is bounded by MaxDepth without a
// check here: encoding/json refuses to read the token that opens container
// MaxDepth+1, so the Token call below fails before the frame that would
// exceed it is entered.
func (d *Decoder) preflightValue() error {
	t, err := d.dec.Token()
	if err != nil {
		return err
	}
	delim, isDelim := t.(json.Delim)
	if !isDelim {
		return nil
	}
	if err := d.preflightContainer(delim); err != nil {
		return err
	}
	return d.Close()
}

// preflightContainer traverses the members of a container whose opening
// delimiter preflightValue has read. The closing delimiter stays
// preflightValue's, so container framing has one owner.
func (d *Decoder) preflightContainer(delim json.Delim) error {
	switch delim {
	case '{':
		return d.preflightObject()
	case '[':
		for d.dec.More() {
			if err := d.preflightValue(); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

// preflightObject consumes an object's members after its opening delimiter,
// failing on the first repeated key. Keys are held in a fold-canonicalized
// set, so a key-dense object costs O(keys) rather than the O(keys^2) an
// EqualFold scan over the accumulated keys would.
func (d *Decoder) preflightObject() error {
	seen := make(map[string]struct{})
	for d.dec.More() {
		key, err := d.Key()
		if err != nil {
			return err
		}
		folded := foldKey(key)
		if _, dup := seen[folded]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateKey, keySnippet(key))
		}
		seen[folded] = struct{}{}
		if err := d.preflightValue(); err != nil {
			return err
		}
	}
	return nil
}

// foldKey canonicalizes each rune to the smallest member of its simple
// case-folding orbit, so map equality on the result is exactly
// strings.EqualFold equality without the per-key linear scan.
func foldKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		c := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < c {
				c = f
			}
		}
		b.WriteRune(c)
	}
	return b.String()
}

// keySnippet truncates an untrusted key to maxKeySnippet bytes on a rune
// boundary, so the error text stays bounded and valid UTF-8.
func keySnippet(key string) string {
	if len(key) <= maxKeySnippet {
		return key
	}
	cut := maxKeySnippet
	for cut > 0 && !utf8.RuneStart(key[cut]) {
		cut--
	}
	return key[:cut]
}
