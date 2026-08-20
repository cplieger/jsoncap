# jsoncap

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/jsoncap.svg)](https://pkg.go.dev/github.com/cplieger/jsoncap)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/jsoncap)](https://github.com/cplieger/jsoncap/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/jsoncap/badges/coverage.json)](https://github.com/cplieger/jsoncap/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/jsoncap/badges/mutation.json)](https://github.com/cplieger/jsoncap/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/14179/badge)](https://www.bestpractices.dev/projects/14179)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/jsoncap/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/jsoncap)

> Bound what an untrusted JSON decode costs, before allocation scales with hostile input

A standalone, stdlib-only Go library for programs that decode JSON they did not write: a third-party API response, an upstream service's reply, a cached document from somewhere else.

A byte cap on the response body is not a bound on decoding. `json.Unmarshal` materializes the entire decoded value before any caller-side count check can run, so compact serialized elements amplify a wire-capped body into decoded structs and slice backing arrays far past its own size. `[0,0,0,` repeated fills a few kilobytes of wire and expands into a slice whose backing array is many times larger.

`jsoncap` closes that gap by moving the count check in front of the allocation. A `Decoder` walks the token stream and lets the caller enforce every cardinality cap _before_ an element is decoded, so a hostile element count is refused rather than absorbed.

The building blocks reproduce `encoding/json`'s observable semantics exactly, so a schema decoder built from them is a drop-in for `json.Unmarshal` on well-formed input.

## Install

```sh
go get github.com/cplieger/jsoncap/v2@latest
```

## Usage

A schema decoder replaces `json.Unmarshal` with a token walk. Each container carries its own cardinality cap, and the whole document shares one aggregate element budget:

```go
dec := jsoncap.NewDecoder(r, 10_000) // aggregate element budget

var rd reportDecoder
if err := dec.Object(func(key string) error { return rd.field(dec, key) }); err != nil {
	return err
}
if err := dec.End(); err != nil {
	return err // trailing data
}
```

`Array` decodes one array under both bounds, refusing the element that would cross either, so the slice never grows past what the caller allowed:

```go
dec := jsoncap.NewDecoder(bytes.NewReader(data), 0)
records, err := dec.Array(nil, maxEnvelopeErrors, "errors",
	func(e *gqlError) error { return dec.Decode(e) })
```

Match the bounds by class:

```go
if errors.Is(err, jsoncap.ErrArrayCap) {
	// one array exceeded its own cap; the wrapping error names it
}
if errors.Is(err, jsoncap.ErrElementBudget) {
	// the document exhausted the aggregate budget across every container
}
```

For a decoder that must retain only part of a large array, walk it element by element instead of through `Array`, which materializes every element into one slice:

```go
if ok, err := dec.Open(json.Delim('[')); err != nil || !ok {
	return err
}
for dec.More() {
	var g group
	if err := dec.Decode(&g); err != nil {
		return err
	}
	// keep what you need, drop the rest
}
return dec.Close()
```

`Preflight` is the other half of the concern: a standalone pass that rejects a body whose _structure_ is ambiguous, before any decode runs. A repeated object key is the case it exists for, because `json.Unmarshal` silently resolves one to the last occurrence and destroys the evidence that it happened:

```go
if err := jsoncap.Preflight(bytes.NewReader(body)); err != nil {
	return err // jsoncap.ErrDuplicateKey on a repeated key
}
```

`Preflight` and the `Decoder` deliberately disagree on duplicate keys, and that is the design. `Object` and `Array` reproduce `Unmarshal`'s merge semantics because a schema decoder must match the stdlib. `Preflight` fails closed because a caller that cannot tolerate the ambiguity should reject the body outright.

## API

- `NewDecoder(r io.Reader, elementBudget int) *Decoder`: a token walker over one JSON value. `elementBudget` bounds the total elements and map entries decoded across every container. A non-positive budget disables the aggregate bound; the per-container caps still apply.
- `(*Decoder).Array[T](prior, maxElems, what, decodeElem) ([]T, error)`: decodes one array, checking the per-array cap and the aggregate budget before each element costs anything. Reproduces `Unmarshal`'s null handling (nil slice), empty-array handling (empty non-nil slice), and duplicate-key lifecycle.
- `(*Decoder).Map[V](prior, maxEntries, what, decodeValue) (map[string]V, error)`: the same for a JSON object decoded into a Go map, charged against the same budget. Map semantics follow `Unmarshal`'s, where a duplicate key replaces with a fresh zero value rather than merging field-wise.
- `Decoder.Object(field func(key string) error) error`: walks an object, dispatching each key to the caller. Match keys with `strings.EqualFold` to reproduce `Unmarshal`'s case-insensitive field fallback.
- `Decoder.Decode(v any) error`: decodes one scalar or value through `json.Decoder.Decode`, for stdlib-identical type handling.
- `Decoder.Skip() error`: token-skips an unknown field without materializing it.
- `Decoder.Open(delim json.Delim) (ok bool, err error)`, `More() bool`, `Key() (string, error)`, `Close() error`, `End() error`, `Elements() int`: the lower-level walk, for a caller assembling a shape the helpers do not cover.
- `Preflight(r io.Reader) error`: one structural pass rejecting a repeated object key. Run it over the whole body, then decode.
- `MaxDepth`: the nesting ceiling every walk here observes, which `encoding/json` provides and enforces.
- `ErrElementBudget`, `ErrArrayCap`, `ErrMapCap`, `ErrDuplicateKey`: matched with `errors.Is` through the wrapping errors the budget and cap checks return.

## Design notes

- **The check goes in front of the allocation, which is the whole point.** A cap applied after `json.Unmarshal` returns has already paid for the decode it was meant to prevent. Every bound here is tested before the element it governs costs anything.
- **Two caps, because they answer different questions.** A per-container cap is a schema statement ("at most 500 items"). The aggregate budget is a document statement, and it is what stops a body that spreads a hostile total across many small, individually-legal containers.
- **Stdlib parity is a contract, not an aspiration.** Null handling, empty containers, duplicate-key merge, case-insensitive field fallback, and unknown-field skipping all reproduce `encoding/json`'s observable behavior, verified by a fuzz target that compares every accepted value against `json.Unmarshal`. A guard that quietly decodes differently from the stdlib is a correctness bug wearing a security feature's clothes.
- **The walk is never looser than the stdlib.** The fuzz parity target asserts the direction explicitly: anything `json.Unmarshal` rejects, this rejects too.
- **`Preflight` fails closed where the `Decoder` merges.** Not an inconsistency: the two serve callers with different tolerances, and the doc comment on each says which.
- **Skipping runs with `UseNumber`.** Skipping an unknown field never converts its numbers through `float64`, which would reject syntactically valid values like `1e1000` that `Unmarshal`'s own field skipping accepts.
- **Errors carry a bound and a name, never document bytes.** The wrapping error names the container through the `what` argument. `ErrDuplicateKey` carries a bounded, quoted snippet of the offending key, and nothing else from the body: an unbounded excerpt on its way to a log line is the amplification this library exists to stop, reintroduced through its own diagnostics.
- **Nesting needs no pass of its own.** `encoding/json` bounds depth at `MaxDepth` for every walk here, `Preflight` included. Since Go 1.27 that comes from `encoding/json/v2`'s `jsontext` decoder, which refuses to read the token opening container `MaxDepth+1`.

## Unsupported by Design

- **Streaming a document larger than memory.** The `Decoder` walks a token stream and never holds the whole body, but the caps are per-document: a caller wanting to process an unbounded stream of independent values loops over `Decode` itself.
- **Schema validation.** The library bounds cost and reproduces stdlib semantics. Whether a field is required, or a number in range, is the caller's contract at the caller's decode site.
- **Per-key cardinality.** "At most N of key X" needs the caller's vocabulary and reads better with a name from it. The per-container caps and the aggregate budget cover the vocabulary-free totals.
- **Invalid UTF-8.** `json.Unmarshal` replaces invalid bytes with the replacement rune, and so does this. A caller that needs byte-exact text validates it separately.
- **A zero value that bounds nothing is deliberate here, and is the opposite of the sibling.** A non-positive `elementBudget` disables the aggregate bound. [`xmlx`](https://github.com/cplieger/xmlx) treats a non-positive bound as a configuration error instead, on the reasoning that a bounds library whose zero value bounds nothing is the failure it exists to prevent. That is the better posture and this library is expected to move to it; until it does, pass a real budget.

## Contributing

See [CONTRIBUTING.md](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md).

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).
