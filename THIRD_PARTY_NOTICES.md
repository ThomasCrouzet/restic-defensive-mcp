# Third-party notices

## Direct dependencies

### github.com/modelcontextprotocol/go-sdk

- License: Apache-2.0 / MIT (see upstream LICENSE)
- Use: MCP protocol server and client (stdio)

### gopkg.in/yaml.v3

- License: MIT / Apache-2.0 dual (see upstream)
- Use: YAML configuration parsing

## External binary (not bundled)

### restic (https://restic.net)

- License: BSD-2-Clause
- Use: invoked as an external process; not linked into this binary

Run `go list -m all` and consult each module's LICENSE for the full dependency tree.
