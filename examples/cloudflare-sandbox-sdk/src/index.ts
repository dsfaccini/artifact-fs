import { Files, SandboxFileError } from '@cloudflare/sandbox';
import { DurableObject } from 'cloudflare:workers';

type Env = WorkerBindings & {
  readonly SANDBOX_API_TOKEN?: string;
};

type MountRequest = {
  sandboxId?: string;
  remote?: string;
  branch?: string;
};

type MountConfig = {
  sandboxId: string;
  remote: string;
  branch: string;
  repoName: string;
  mountPath: string;
  env: Record<string, string>;
};

type StoredMountMetadata = {
  remote: string;
  branch: string;
  repoName: string;
  mountPath: string;
};

const DEFAULT_REMOTE = 'https://github.com/cloudflare/sandbox-sdk.git';
const DEFAULT_BRANCH = 'main';
const DEFAULT_MOUNT_ROOT = '/workspace/mnt';
const DEFAULT_ARTIFACT_FS_ROOT = '/tmp/artifact-fs';
const DEFAULT_METADATA_FILE = '/workspace/.artifact-fs-mount';
const MOUNT_SCRIPT = '/usr/local/bin/mount-artifact-fs-repo';
const MAX_REQUEST_BODY_BYTES = 16 * 1024;
const MOUNT_CONFLICT_EXIT_CODE = 42;
const INACTIVITY_TIMEOUT_MS = 15 * 60 * 1000;

type CommandResult = { exitCode: number; stdout: string; stderr: string };

type MountResult =
  | { conflict: string }
  | {
      sandboxId: string;
      remote: string;
      branch: string;
      repoName: string;
      mountPath: string;
      bootstrapLog: string;
      head: string | null;
      gitStatus: string | null;
      artifactFsStatus: string;
    };

type StatusResult =
  | { missing: true }
  | { conflict: string }
  | {
      sandboxId: string;
      remote: string;
      branch: string;
      repoName: string;
      mountPath: string;
      head: string | null;
      gitStatus: string | null;
      artifactFsStatus: string;
    };

export class Sandbox extends DurableObject<Env> {
  private setup: Promise<void> | null = null;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);

    const container = ctx.container;
    if (container?.running) {
      void ctx.blockConcurrencyWhile(() =>
        container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS)
      );
    }
  }

  async mount(config: MountConfig): Promise<MountResult> {
    await this.ensureRunning(config.env);

    const existingMetadata = await this.readMountMetadata();
    const mountConflict = compareMountMetadata(existingMetadata, config);
    if (mountConflict) {
      return { conflict: mountConflict };
    }

    let bootstrap: CommandResult;
    try {
      bootstrap = await this.runCommand(
        ['timeout', '--kill-after=5s', '120s', MOUNT_SCRIPT],
        config.env
      );
    } catch (error) {
      await this.destroyAfterFailure();
      throw error;
    }

    if (bootstrap.exitCode === MOUNT_CONFLICT_EXIT_CODE) {
      return {
        conflict: 'Sandbox is already initialized for a different repo or branch'
      };
    }

    if (bootstrap.exitCode !== 0) {
      const commandError = new SandboxCommandError(
        'ArtifactFS bootstrap failed',
        MOUNT_SCRIPT,
        bootstrap.exitCode
      );
      await this.destroyAfterFailure();
      throw commandError;
    }

    const repo = await this.collectMountedRepoState(
      config.repoName,
      config.mountPath,
      config.env
    );

    return {
      sandboxId: config.sandboxId,
      remote: config.remote,
      branch: config.branch,
      repoName: config.repoName,
      mountPath: config.mountPath,
      bootstrapLog: bootstrap.stdout.trim(),
      ...repo
    };
  }

  async status(
    statusConfig: ReturnType<typeof buildStatusConfig>
  ): Promise<StatusResult> {
    await this.ensureRunning(buildCommandEnv());
    const metadata = await this.readMountMetadata();
    if (!metadata) {
      return { missing: true };
    }

    const mountConflict = compareMountMetadata(metadata, {
      remote: statusConfig.remote,
      branch: statusConfig.branch,
      repoName: metadata.repoName,
      mountPath: metadata.mountPath
    });
    if (mountConflict) {
      return { conflict: mountConflict };
    }

    const env = buildCommandEnv(metadata.remote, metadata.branch);
    const repo = await this.collectMountedRepoState(
      metadata.repoName,
      metadata.mountPath,
      env
    );

    return {
      sandboxId: statusConfig.sandboxId,
      remote: metadata.remote,
      branch: metadata.branch,
      repoName: metadata.repoName,
      mountPath: metadata.mountPath,
      ...repo
    };
  }

  private get container(): Container {
    const container = this.ctx.container;
    if (!container) {
      throw new Error('No container is configured for this Durable Object');
    }
    return container;
  }

  private get files(): Files {
    return new Files(this.container);
  }

  private ensureRunning(env: Record<string, string>): Promise<void> {
    if (this.setup === null || !this.container.running) {
      this.setup = this.startContainer(env).catch((error: unknown) => {
        this.setup = null;
        throw error;
      });
    }
    return this.setup;
  }

  private async startContainer(env: Record<string, string>): Promise<void> {
    const container = this.container;
    if (!container.running) {
      container.start({
        image: container.images.artifactfs,
        instance: 'standard-1',
        env,
        enableInternet: true
      });
    }

    try {
      await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
    } catch (error) {
      await this.destroyAfterFailure();
      throw error;
    }
  }

  private async destroyAfterFailure(): Promise<void> {
    try {
      await this.container.destroy();
    } catch (destroyError) {
      console.error('sandbox cleanup failed', destroyError);
    }
  }

  private async runCommand(
    command: string[],
    env: Record<string, string>
  ): Promise<CommandResult> {
    await this.ensureRunning(env);
    const process = await this.container.exec(command, {
      cwd: '/workspace',
      env
    });
    const output = await process.output();
    const decoder = new TextDecoder();
    return {
      exitCode: output.exitCode,
      stdout: decoder.decode(output.stdout),
      stderr: decoder.decode(output.stderr)
    };
  }

  private async readMountMetadata(): Promise<StoredMountMetadata | null> {
    try {
      const response = await this.files.readFile(DEFAULT_METADATA_FILE);
      return parseMetadataFile(await response.text());
    } catch (error) {
      if (SandboxFileError.is(error) && error.code === 'ENOENT') {
        return null;
      }
      throw error;
    }
  }

  private async collectMountedRepoState(
    repoName: string,
    mountPath: string,
    env: Record<string, string>
  ) {
    const artifactFsStatus = await this.runChecked(
      ['artifact-fs', 'status', '--name', repoName],
      'Could not read ArtifactFS status',
      env
    );

    const mounted = /(^|\s)state=mounted(\s|$)/.test(artifactFsStatus.stdout);
    if (!mounted) {
      return {
        head: null,
        gitStatus: null,
        artifactFsStatus: artifactFsStatus.stdout.trim()
      };
    }

    const [head, gitStatus] = await Promise.all([
      this.runChecked(
        ['git', '-C', mountPath, 'rev-parse', 'HEAD'],
        'Could not read mounted HEAD',
        env
      ),
      this.runChecked(
        ['git', '-C', mountPath, 'status', '--short', '--branch'],
        'Could not read mounted git status',
        env
      )
    ]);

    return {
      head: head.stdout.trim(),
      gitStatus: gitStatus.stdout.trim(),
      artifactFsStatus: artifactFsStatus.stdout.trim()
    };
  }

  private async runChecked(
    command: string[],
    message: string,
    env: Record<string, string>
  ): Promise<CommandResult> {
    const result = await this.runCommand(command, env);
    if (result.exitCode !== 0) {
      throw new SandboxCommandError(message, command.join(' '), result.exitCode);
    }
    return result;
  }
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (request.method === 'GET' && url.pathname === '/') {
      return new Response(helpText(), {
        headers: { 'Content-Type': 'text/plain; charset=utf-8' }
      });
    }

    if (request.method === 'POST' && url.pathname === '/mount') {
      return handleMount(request, env);
    }

    if (request.method === 'GET' && url.pathname === '/status') {
      return handleStatus(request, env);
    }

    return new Response('Not found', { status: 404 });
  }
} satisfies ExportedHandler<Env>;

async function handleMount(request: Request, env: Env): Promise<Response> {
  try {
    await authorizeRequest(request, env);
    const body = await parseMountRequest(request);
    const config = buildMountConfig(body.sandboxId, body.remote, body.branch);
    const sandbox = env.Sandbox.getByName(config.sandboxId);
    const result = await sandbox.mount(config);
    if ('conflict' in result) {
      return Response.json({ error: result.conflict }, { status: 409 });
    }
    return Response.json(result);
  } catch (error) {
    return errorResponse(error, request);
  }
}

async function handleStatus(request: Request, env: Env): Promise<Response> {
  try {
    await authorizeRequest(request, env);
    const url = new URL(request.url);
    const sandboxId = url.searchParams.get('sandboxId');
    if (!sandboxId) {
      return Response.json(
        { error: 'Missing sandboxId query parameter' },
        { status: 400 }
      );
    }

    const config = buildStatusConfig(
      sandboxId,
      url.searchParams.get('remote'),
      url.searchParams.get('branch')
    );
    const sandbox = env.Sandbox.getByName(config.sandboxId);
    const result = await sandbox.status(config);
    if ('missing' in result) {
      return Response.json(
        {
          error:
            'No active mount found. The sandbox may have slept; POST /mount again to recreate it.'
        },
        { status: 404 }
      );
    }

    if ('conflict' in result) {
      return Response.json({ error: result.conflict }, { status: 409 });
    }
    return Response.json(result);
  } catch (error) {
    return errorResponse(error, request);
  }
}

async function parseMountRequest(request: Request): Promise<MountRequest> {
  let body: unknown;

  try {
    body = JSON.parse(await readRequestBody(request));
  } catch (error) {
    if (error instanceof UserError) {
      throw error;
    }
    throw new UserError('Request body must be valid JSON', 400);
  }

  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    throw new UserError('Request body must be a JSON object', 400);
  }

  const { sandboxId, remote, branch } = body as Record<string, unknown>;
  validateOptionalString('sandboxId', sandboxId);
  validateOptionalString('remote', remote);
  validateOptionalString('branch', branch);

  return {
    sandboxId,
    remote,
    branch
  };
}

async function readRequestBody(request: Request): Promise<string> {
  if (!request.body) {
    return '';
  }

  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let totalBytes = 0;

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }

      totalBytes += value.byteLength;
      if (totalBytes > MAX_REQUEST_BODY_BYTES) {
        await reader.cancel();
        throw new UserError('Request body is too large', 413);
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }

  const body = new Uint8Array(totalBytes);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(body);
}

function buildMountConfig(
  sandboxIdInput?: string | null,
  remoteInput?: string | null,
  branchInput?: string | null
): MountConfig {
  const remote = normalizeRemote(remoteInput ?? DEFAULT_REMOTE);
  const branch = (branchInput ?? DEFAULT_BRANCH).trim() || DEFAULT_BRANCH;
  validateBranch(branch);
  const repoName = inferRepoName(remote);
  const sandboxId = sandboxIdInput
    ? normalizeRequestedSandboxId(sandboxIdInput)
    : normalizeSandboxId(crypto.randomUUID());
  const mountPath = `${DEFAULT_MOUNT_ROOT}/${repoName}`;

  return {
    sandboxId,
    remote,
    branch,
    repoName,
    mountPath,
    env: buildCommandEnv(remote, branch)
  };
}

function buildStatusConfig(
  sandboxIdInput: string,
  remoteInput?: string | null,
  branchInput?: string | null
) {
  const branch = branchInput?.trim() || undefined;
  if (branch) {
    validateBranch(branch);
  }

  return {
    sandboxId: normalizeRequestedSandboxId(sandboxIdInput),
    remote: remoteInput ? normalizeRemote(remoteInput) : undefined,
    branch
  };
}

function buildCommandEnv(
  remote = DEFAULT_REMOTE,
  branch = DEFAULT_BRANCH
): Record<string, string> {
  return {
    MOUNT_GIT_REMOTE: remote,
    MOUNT_GIT_BRANCH: branch,
    ARTIFACT_FS_ROOT: DEFAULT_ARTIFACT_FS_ROOT,
    MOUNT_ROOT: DEFAULT_MOUNT_ROOT,
    ARTIFACT_FS_MOUNT_METADATA_FILE: DEFAULT_METADATA_FILE
  };
}

function normalizeRemote(value: string): string {
  const remote = value.trim();
  if (remote.length > 2048 || /[\u0000-\u0020\u007f]/.test(remote)) {
    throw new UserError('remote contains invalid characters or is too long', 400);
  }

  if (remote.startsWith('https://') || remote.startsWith('ssh://')) {
    let parsed: URL;
    try {
      parsed = new URL(remote);
    } catch {
      throw new UserError('remote must be a valid Git URL', 400);
    }

    if (
      (parsed.protocol === 'https:' && (parsed.username || parsed.password)) ||
      (parsed.protocol === 'ssh:' && parsed.password)
    ) {
      throw new UserError(
        'remote must not include credentials; use credential helpers or SSH auth instead',
        400
      );
    }

    if (parsed.search || parsed.hash) {
      throw new UserError(
        'remote must not include query parameters or fragments',
        400
      );
    }

    if (!parsed.pathname || parsed.pathname === '/') {
      throw new UserError('remote must include a repository path', 400);
    }

    return remote;
  }

  if (/^[^@:\s]+@[^:\s]+:[^\s?#]+$/.test(remote)) {
    return remote;
  }

  throw new UserError('remote must be an HTTPS or SSH Git URL', 400);
}

function inferRepoName(remote: string): string {
  const trimmed = remote.replace(/\/+$/, '').replace(/\.git$/, '');
  const lastSeparator = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf(':'));
  const repoName = trimmed.slice(lastSeparator + 1);

  if (
    !repoName ||
    repoName.length > 100 ||
    !/^[a-zA-Z0-9._-]+$/.test(repoName) ||
    repoName === '.' ||
    repoName === '..'
  ) {
    throw new UserError(
      'repository name must use 1-100 letters, numbers, dots, hyphens, or underscores',
      400
    );
  }

  return repoName;
}

function normalizeSandboxId(value: string): string {
  return value.trim().toLowerCase();
}

function normalizeRequestedSandboxId(value: string): string {
  const normalized = normalizeSandboxId(value);

  if (!normalized) {
    throw new UserError('sandboxId must be 1-63 characters long', 400);
  }

  if (normalized.length > 63) {
    throw new UserError('sandboxId must be 1-63 characters long', 400);
  }

  if (normalized.startsWith('-') || normalized.endsWith('-')) {
    throw new UserError('sandboxId cannot start or end with hyphens', 400);
  }

  if (!/^[a-z0-9.-]+$/.test(normalized)) {
    throw new UserError(
      'sandboxId may contain only letters, numbers, dots, and hyphens',
      400
    );
  }

  if (!/[a-z0-9]/.test(normalized)) {
    throw new UserError('sandboxId must include at least one letter or number', 400);
  }

  if (
    ['www', 'api', 'admin', 'root', 'system', 'cloudflare', 'workers'].includes(
      normalized
    )
  ) {
    throw new UserError('sandboxId is reserved', 400);
  }

  return normalized;
}

function compareMountMetadata(
  existing: StoredMountMetadata | null,
  requested: {
    remote?: string;
    branch?: string;
    repoName: string;
    mountPath: string;
  }
): string | null {
  if (!existing) {
    return null;
  }

  if (requested.remote && existing.remote !== requested.remote) {
    return `Sandbox is mounted for ${existing.remote}, not ${requested.remote}`;
  }

  if (requested.branch && existing.branch !== requested.branch) {
    return `Sandbox is mounted for branch ${existing.branch}, not ${requested.branch}`;
  }

  if (
    existing.repoName !== requested.repoName ||
    existing.mountPath !== requested.mountPath
  ) {
    return 'Sandbox metadata does not match the requested mount layout';
  }

  return null;
}

function parseMetadataFile(content: string): StoredMountMetadata {
  const values = new Map<string, string>();

  for (const line of content.split('\n')) {
    if (!line) {
      continue;
    }
    const index = line.indexOf('=');
    if (index === -1) {
      continue;
    }
    const key = line.slice(0, index);
    const rawValue = line.slice(index + 1).trim();
    values.set(key, rawValue);
  }

  const remote = values.get('MOUNTED_REMOTE');
  const branch = values.get('MOUNTED_BRANCH');
  const repoName = values.get('MOUNTED_REPO_NAME');
  const mountPath = values.get('MOUNTED_MOUNT_PATH');

  if (!remote || !branch || !repoName || !mountPath) {
    throw new Error('Mount metadata is missing required fields');
  }

  return { remote, branch, repoName, mountPath };
}

function validateOptionalString(
  name: string,
  value: unknown
): asserts value is string | undefined {
  if (value !== undefined && typeof value !== 'string') {
    throw new UserError(`${name} must be a string`, 400);
  }
}

function validateBranch(branch: string): void {
  const invalidBase =
    !branch ||
    branch === '@' ||
    branch.startsWith('-') ||
    branch.startsWith('/') ||
    branch.endsWith('/') ||
    branch.endsWith('.') ||
    branch.includes('..') ||
    branch.includes('//') ||
    branch.includes('@{') ||
    /[\x00-\x20~^:?*[\\]/.test(branch);

  if (invalidBase) {
    throw new UserError('branch must be a valid Git branch name', 400);
  }

  for (const component of branch.split('/')) {
    if (!component || component.startsWith('.') || component.endsWith('.lock')) {
      throw new UserError('branch must be a valid Git branch name', 400);
    }
  }
}

function helpText(): string {
  return [
    'ArtifactFS + Cloudflare Sandbox SDK example',
    '',
    'POST /mount with JSON: {"sandboxId":"demo","remote":"https://github.com/cloudflare/sandbox-sdk.git","branch":"main"}',
    'GET  /status?sandboxId=demo&remote=https://github.com/cloudflare/sandbox-sdk.git',
    '',
    'The Docker image includes ArtifactFS and a bootstrap script that mounts the requested repo inside /workspace/mnt/<repo-name>.'
  ].join('\n');
}

function errorResponse(error: unknown, request: Request): Response {
  if (error instanceof UserError) {
    const headers =
      error.status === 401 ? { 'WWW-Authenticate': 'Bearer' } : undefined;
    return Response.json(
      { error: error.message },
      { status: error.status, headers }
    );
  }

  logError('request failed', error, request);
  return Response.json({ error: 'Internal server error' }, { status: 500 });
}

function logError(message: string, error: unknown, request: Request): void {
  const details =
    error instanceof SandboxCommandError
      ? {
          error: error.message,
          command: error.command,
          exitCode: error.exitCode
        }
      : {
          error: error instanceof Error ? error.message : String(error)
        };

  console.error(
    JSON.stringify({
      message,
      method: request.method,
      path: new URL(request.url).pathname,
      ...details
    })
  );
}

async function authorizeRequest(request: Request, env: Env): Promise<void> {
  const configuredToken = env.SANDBOX_API_TOKEN ?? '';
  if (!configuredToken) {
    throw new Error('SANDBOX_API_TOKEN is not configured');
  }

  const header = (request.headers.get('authorization') ?? '').trim();
  const match = /^Bearer ([^\s]+)$/i.exec(header);
  const tokenMatches = await timingSafeEqual(match?.[1] ?? '', configuredToken);
  if (!match || !tokenMatches) {
    throw new UserError('Unauthorized', 401);
  }
}

async function timingSafeEqual(left: string, right: string): Promise<boolean> {
  const encoder = new TextEncoder();
  const [leftHash, rightHash] = await Promise.all([
    crypto.subtle.digest('SHA-256', encoder.encode(left)),
    crypto.subtle.digest('SHA-256', encoder.encode(right))
  ]);
  return crypto.subtle.timingSafeEqual(leftHash, rightHash);
}

class SandboxCommandError extends Error {
  constructor(
    message: string,
    readonly command: string,
    readonly exitCode: number
  ) {
    super(message);
  }
}

class UserError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
  }
}
