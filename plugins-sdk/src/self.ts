import * as plugins from "./index";

// A plugin's `import … from "@repogo/plugins"` gets this runtime's own copy,
// not the one in the project's node_modules: one version, matching the host,
// and one zod, so its schemas and MethodError are the ones the runtime reads.
// A virtual module, since a runtime resolve hook doesn't see bare package names.
Bun.plugin({
  name: "@repogo/plugins",
  setup(build) {
    build.module("@repogo/plugins", () => ({ exports: plugins, loader: "object" }));
  },
});
