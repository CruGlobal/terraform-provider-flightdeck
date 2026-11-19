# Flightdeck's OpenAPI document

`flightdeck-v1.json` is Flightdeck's description of its REST API, the
OpenAPI 3.1 document a Flightdeck instance serves at
`GET /api/v1/openapi.json`. The fake in `internal/flightdecktest` checks
every answer it sends against it (`openapi.go`), so a test fails when the
fake says something the real API would not.

- **Copied from:** Flightdeck commit `8c50b347`
- **Copied on:** 2026-11-19

The copy differs from what Flightdeck serves in two ways, both made by
`refresh/`: the host names in its `servers` list are replaced with
`example.com` names wherever they appear, and the JSON is indented. Don't
edit it by hand. If it's wrong, fix it in Flightdeck and refresh it here.

## Refreshing it

The document needs no token. From the repository root:

```sh
curl -sf https://flightdeck.example.com/api/v1/openapi.json \
  | go run ./internal/flightdecktest/openapi/refresh \
  > internal/flightdecktest/openapi/flightdeck-v1.json
```

Use your Flightdeck instance's address in place of
`flightdeck.example.com`. Then update the commit and date above, run
`task test`, and fix the fake wherever it now disagrees with the document.
