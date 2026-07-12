# The OpenAPI contract

The spec is generated from the Go types themselves, so it cannot drift from the implementation.

```bash
make spec           # writes bin/openapi.json
```

It is also served live at `/openapi.json`, `/openapi.yaml`, and rendered at `/docs`.

Because the clients are separate repositories, **the spec is a public interface.** `muallim-web` generates its typed client from this document, so a breaking change here fails that build rather than production. Renaming an `OperationID` or narrowing a field breaks consumers you cannot see — a WordPress plugin, a mobile app, an LTI tool.

Every error response is an RFC 9457 problem document; see [architecture.md](architecture.md#errors).
