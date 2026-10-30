# Cloudflare Sandbox SDK example

This example combines the [Cloudflare Sandbox SDK](https://developers.cloudflare.com/sandbox/) with ArtifactFS. The Worker starts an isolated container and mounts a public Git repository at `/workspace/mnt/<repo>`.

It is useful for agents and build jobs that need a normal working tree without eagerly downloading every blob. ArtifactFS uses FUSE to fetch file content on demand.

It includes an authenticated mount API, a status endpoint, and a custom Sandbox image. The single bearer token keeps this example focused; it is not a multi-tenant authorization design.

This uses Sandbox SDK 1.0 and the Durable Object scheduling policy, and deploys a separate Worker (`artifact-fs-sandbox-sdk-v1-example`) with a new namespace. To move an existing 0.x deployment, follow Cloudflare's [side-by-side migration guide](https://developers.cloudflare.com/sandbox/sdk/migrate/move-side-by-side/). Account limits apply; this example has no per-application container instance cap.

You need Docker, Node.js 26, and a Workers Paid plan.

```bash
cd examples/cloudflare-sandbox-sdk
npm ci
cp .dev.vars.example .dev.vars
npm run dev
```

Mount the default repository:

```bash
curl -X POST http://localhost:8787/mount \
  -H 'authorization: Bearer local-dev-token' \
  -H 'content-type: application/json' \
  -d '{"sandboxId":"demo"}'
```

Check the active mount:

```bash
curl -H 'authorization: Bearer local-dev-token' \
  'http://localhost:8787/status?sandboxId=demo'
```

The container sleeps after 15 minutes of inactivity. Repeat `POST /mount` to recreate an expired mount.

Deploy with a production bearer token:

```bash
npx wrangler secret put SANDBOX_API_TOKEN
npm run deploy
```

For production, derive sandbox IDs from authenticated identities and add rate limits. Private repositories need a credential flow that does not embed secrets in the Git URL.
