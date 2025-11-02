# JSON Serialization Demo

This example demonstrates how to use JSON marshaling and unmarshaling with ClassAds.

## Features Demonstrated

1. **Marshal ClassAd to JSON** - Convert a ClassAd into JSON format
2. **Expression Serialization** - See how expressions are serialized with the special `\/Expr(<expression>)\/` format
3. **Lists and Nested ClassAds** - Handle complex nested structures
4. **Unmarshal JSON to ClassAd** - Convert JSON back into a ClassAd
5. **Round-trip Serialization** - Verify that marshal→unmarshal preserves data

## JSON Format

The ClassAd JSON serialization follows these rules:

- **Simple literals** (integers, reals, strings, booleans) → JSON values
- **undefined** → JSON `null`
- **Lists** → JSON arrays
- **Nested ClassAds** → JSON objects
- **Complex expressions** → Special format: `"/Expr(<expression>)/"` (appears as `"\/Expr(<expression>)\/"` in JSON due to `/` escaping)

### Expression Format Example

A ClassAd like:
```
[x = 5; y = x + 3]
```

Serializes to JSON as:
```json
{
  "x": 5,
  "y": "\/Expr(x + 3)\/"
}
```

The format is `/Expr(...)/)` where the forward slashes are escaped in JSON as `\/`.

When unmarshaled back, the expression `x + 3` is preserved and will correctly evaluate to `8` in the context of the ClassAd.

## Running the Demo

```bash
go run main.go
```

## Sample Output

The demo shows:
- Simple value serialization
- Expression handling with the special format
- Lists of both values and expressions
- Nested ClassAds as JSON objects
- JSON-to-ClassAd unmarshaling with expression evaluation
- Round-trip verification

## Use Cases

This JSON serialization is useful for:
- **REST APIs** - Send/receive ClassAds over HTTP
- **Configuration files** - Store ClassAds in JSON format
- **Interoperability** - Exchange ClassAds with non-Go systems
- **Database storage** - Store ClassAds in JSON columns
- **Logging** - Serialize ClassAds for structured logging
