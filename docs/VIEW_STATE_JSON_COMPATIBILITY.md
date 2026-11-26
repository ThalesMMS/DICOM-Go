# ViewState JSON on Go 1.22 and newer

`MPRViewState`, `VRViewState`, and `MPRPresentationState` implement
`json.Marshaler` so their optional fields have the same zero-value omission on
Go 1.22 and newer toolchains. The [Go 1.24 release notes](https://go.dev/doc/go1.24#encodingjsonpkgencodingjson)
introduced the `omitzero` tag used by these fields. Go 1.22 does not implement
that option.

The existing field names, order, numeric representations, contract versions,
and header sizes are preserved. Serialization does not validate or upgrade a
request: use `ValidateViewState` separately. There is no custom JSON decoder;
`json.Unmarshal` retains its usual merge behavior for a reused destination and
ignores unknown fields unless a caller's decoder requests otherwise.

## Optional fields

The decision is based on the complete Go zero value, including fields excluded
from JSON. It is not based on an enabled flag or the serialized object alone.

| Type | Fields omitted when their Go value is zero |
|---|---|
| `MPRViewState` | `Presentation` |
| `VRViewState` | `Labels`, `Scattering`, `Cut`, `Geometry`, `GradientOpacity`, `IsoSurfaces` |
| `MPRPresentationState` | `Overlay`, `Fusion` |

The V13 DVR `OpacityNormals` boolean separately uses `omitempty`: false is
absent on all supported toolchains; true requires V13 validation. Older
requests keep scalar normals. See the opacity-normal contract and Windows
GPU qualification.

For example, nil `GradientOpacity` and `IsoSurfaces` are absent, while non-nil
empty slices are present as `[]`. A zero `Geometry.Enabled` with a non-nil
`Depth` or `Color` still emits `Geometry`; the sidecars themselves remain
excluded by `json:"-"`. Likewise, `Labels.Volume` can make `Labels` non-zero
without appearing in JSON. An empty non-nil palette or ramp can make its
enclosing optional struct non-zero while its own `omitempty` field stays
absent. Disabled settings are therefore not always zero values.

Negative-zero floating point values count as zero for optional-field omission.
Nonzero values are encoded normally, with no clamping, normalization, or
change to slice ownership. The methods copy struct values and slice headers;
they do not modify or retain caller data. Concurrent marshaling of immutable
values is supported; callers must not mutate shared slices during encoding.

Nil pointers to these types still encode as `null` through `encoding/json`.
The compatibility behavior applies to these public types, their true aliases,
and their values or pointers in named fields of larger objects. A
consumer-defined new type that discards the methods does not inherit this
marshaling contract.

## Anonymous embedding and method promotion

Adding these methods also affects Go method sets. Anonymous embedding of one
of these types can promote `MarshalJSON` to the enclosing struct. In that
case, `encoding/json` calls the promoted method for the entire wrapper; the
method encodes only the embedded receiver, so additional wrapper fields are
not encoded. This differs from the former default encoding of an anonymous
wrapper, and compatibility is not promised for arbitrary such wrappers.

Use a named field to retain the wrapper's additional fields, or supply the
wrapper's own deliberate marshaling implementation:

```go
type Request struct {
    State render.VRViewState
    Extra string
}
```

The wrapper tests cover values and pointers with named fields and the
documented method promotion of anonymous fields for all three types.

## Errors

Non-finite numbers in serialized fields still cause a JSON encoding error.
Non-finite numbers in an ignored sidecar do not introduce a validation error.
Because these types implement `MarshalJSON`, `encoding/json` can wrap an
underlying encoding error in `*json.MarshalerError`. The outer error type and
text are not promised to match the former native encoder. Use `errors.As` to
inspect the underlying error:

```go
var unsupported *json.UnsupportedValueError
if errors.As(err, &unsupported) {
    // A serialized value cannot be represented in JSON.
}
```

The [encoding/json API](https://pkg.go.dev/encoding/json) describes marshaler
errors and field selection. These methods do not change the render validation
error API.

## Finite compatibility evidence

The fixture
[view_state_json_native.json](../render/testdata/view_state_json_native.json)
contains 46 public-API cases captured on the unmodified library commit
`342312b0608b301fa1ff6e9df2d2572508d43ce3` with native Go 1.27.1, before these
methods were added. It stores exact `Marshal`, `MarshalIndent`, and `Encoder`
bytes, with provenance and compact-output SHA256 values. The inputs include
all nine optional fields, zero/nonzero values, nil/empty/populated slices,
ignored sidecars, negative zero, nested requests, and versions V1–V12.

The [compatibility tests](../render/view_state_json_test.go) compare those bytes
for values and pointers, independently assert omission and empty-slice
semantics, and exercise decoder reuse, unsupported values, source immutability,
concurrent readers, and wrapper method promotion. The existing seven
zero-omission tests remain intact.
The fixture is an oracle for these finite cases, not a claim of exhaustive
JSON compatibility for all consumer-defined types or future fields. Changes
to field order must update the contract deliberately, rather than weakening
byte comparisons to semantic equality.

This change addresses library JSON compatibility. It does not change
render mathematics, GPU behavior, desktop workflows, or the minimum Go
directive, and it makes no performance claim.
