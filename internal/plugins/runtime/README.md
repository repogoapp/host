# The plugins runtime a release carries

`bun run build:runtime` in `plugins-sdk` bundles the SDK's `run.ts` and
`worker.ts` here, and the release workflow runs it before building the host,
which embeds this folder (`internal/plugins/runtime.go`). The bundles are
generated and not checked in; a dev build carries none and runs the SDK from
source, through `REPOGO_PLUGINS_SDK`.
