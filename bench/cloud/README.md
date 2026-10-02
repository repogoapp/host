# Cloud project cards

From the host's root, with the Vercel, Fly and Cloudflare CLIs signed in:

```sh
go run ./bench/cloud ~/path/to/projects > /tmp/repogo-cloud-projects.json
```

It runs `cloudprojects.Service.List`, the implementation behind `cloud.projects.list`, on the folder given, prints the result to stdout and the wall time and counts to stderr. Exit 2 means a project couldn't be read or the scan hit an issue; the JSON is still printed.

`List` finds every `.vercel` link, `vercel.json`, `fly.toml` and `wrangler.toml`/`.json`/`.jsonc` under the folder (`cloudscan`, following `.gitignore` inside repositories), then reads each linked project from its provider, at most four at a time:

- Vercel: one `vercel api /v9/projects/<id>` call (about 0.7 s), plus the build log when the latest production deploy failed (about 0.8 s).
- Fly: `flyctl releases` and `flyctl certs list` in parallel (about 0.6 s).
- Cloudflare: `wrangler deployments list` (1.1–1.5 s), with the address from Cloudflare's API in parallel.

Nothing linked means no CLI call.
