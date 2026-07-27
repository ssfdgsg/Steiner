# Contributing to Steiner

Thanks for contributing. Please open an issue first for substantial behavior, API, or architecture changes so the intended design can be discussed before implementation.

## Development setup

Requirements: Go 1.22 and, when changing the web console, a current Node.js LTS release.

```bash
make test
make vet
make build
```

For frontend changes, run `make web` to regenerate `internal/webui/dist`, then include the generated files in the same pull request. The gateway embeds those files at build time.

## Pull requests

- Keep each pull request focused and include tests for behavior changes.
- Run `make fmt`, `make test`, `make vet`, and `make build` before requesting review.
- Do not commit credentials, downloaded models, `web/node_modules`, or locally generated binaries.
- Update the relevant README or API documentation whenever user-visible behavior or configuration changes.

By contributing, you agree that your contributions are licensed under the project's Apache-2.0 license.
