import { join } from "node:path";

export interface AuditAdvisory {
  url: string;
  title: string;
  severity: string;
}

export type AuditReport = Record<string, AuditAdvisory[]>;

export interface DependencyNode {
  package: string;
  version: string;
}

export interface AuditException {
  workspace: string;
  package: string;
  advisory: string;
  cve: string;
  severity: string;
  dependencyPath: DependencyNode[];
  rationale: string;
  owner: string;
  reviewedOn: string;
  expiresOn: string;
}

export interface BunLockfile {
  packages: Record<
    string,
    [
      resolution: string,
      registry: string,
      metadata?: { dependencies?: Record<string, string> },
      integrity?: string,
    ]
  >;
}

function exceptionKey(exception: Pick<AuditException, "workspace" | "package" | "advisory">): string {
  return `${exception.workspace}:${exception.package}:${exception.advisory}`;
}

function advisoryID(url: string): string {
  return url.split("/").filter(Boolean).at(-1) ?? "";
}

function packageRecord(lockfile: BunLockfile, node: DependencyNode) {
  const resolution = `${node.package}@${node.version}`;
  return Object.values(lockfile.packages).find((record) => record[0] === resolution);
}

function validateExceptionMetadata(exception: AuditException, lockfile: BunLockfile, now: Date): string[] {
  const errors: string[] = [];
  const label = exceptionKey(exception);
  const expiry = new Date(`${exception.expiresOn}T23:59:59Z`);

  if (!/^GHSA-[a-z0-9-]+$/.test(exception.advisory)) errors.push(`${label}: invalid GHSA id`);
  if (!/^CVE-\d{4}-\d+$/.test(exception.cve)) errors.push(`${label}: invalid CVE id`);
  if (!exception.rationale || !exception.owner || !exception.reviewedOn) errors.push(`${label}: incomplete review metadata`);
  if (Number.isNaN(expiry.valueOf()) || expiry < now) errors.push(`${label}: exception expired on ${exception.expiresOn}`);
  if (exception.dependencyPath.length < 2) errors.push(`${label}: dependency path is not precise`);

  const leaf = exception.dependencyPath.at(-1);
  if (!leaf || leaf.package !== exception.package) {
    errors.push(`${label}: dependency path does not end at ${exception.package}`);
  }

  for (let i = 0; i < exception.dependencyPath.length; i += 1) {
    const node = exception.dependencyPath[i];
    const record = packageRecord(lockfile, node);
    if (!record) {
      errors.push(`${label}: lockfile does not contain ${node.package}@${node.version}`);
      continue;
    }
    const child = exception.dependencyPath[i + 1];
    if (child && !record[2]?.dependencies?.[child.package]) {
      errors.push(`${label}: lockfile path ${node.package} -> ${child.package} is absent`);
    }
  }
  return errors;
}

export function validateAudit(
  workspace: string,
  report: AuditReport,
  exceptions: AuditException[],
  lockfile: BunLockfile,
  now = new Date(),
): string[] {
  const errors: string[] = [];
  const used = new Set<string>();
  const workspaceExceptions = exceptions.filter((exception) => exception.workspace === workspace);

  for (const [packageName, advisories] of Object.entries(report)) {
    for (const advisory of advisories) {
      const id = advisoryID(advisory.url);
      const match = workspaceExceptions.find(
        (exception) => exception.package === packageName && exception.advisory === id,
      );
      if (!match) {
        errors.push(`${workspace}:${packageName}:${id || advisory.url}: vulnerability is not allowed`);
        continue;
      }
      if (match.severity !== advisory.severity) {
        errors.push(`${exceptionKey(match)}: severity changed from ${match.severity} to ${advisory.severity}`);
      }
      used.add(exceptionKey(match));
    }
  }

  for (const exception of workspaceExceptions) {
    const key = exceptionKey(exception);
    errors.push(...validateExceptionMetadata(exception, lockfile, now));
    if (!used.has(key)) errors.push(`${key}: stale exception; advisory was not reported`);
  }
  return errors;
}

function runBunAudit(workspaceDirectory: string): AuditReport {
  const result = Bun.spawnSync([process.execPath, "audit", "--json"], {
    cwd: workspaceDirectory,
    stdout: "pipe",
    stderr: "pipe",
  });
  const stdout = result.stdout.toString().trim();
  if (!stdout) {
    throw new Error(`bun audit failed in ${workspaceDirectory}: ${result.stderr.toString().trim()}`);
  }
  try {
    return JSON.parse(stdout) as AuditReport;
  } catch (error) {
    throw new Error(`bun audit returned invalid JSON in ${workspaceDirectory}: ${String(error)}`);
  }
}

async function main(): Promise<void> {
  const root = join(import.meta.dir, "..");
  const exceptions = (await Bun.file(join(root, "security/frontend-audit-allowlist.json")).json()) as AuditException[];
  const workspaces = ["web/faire", "website"];
  const errors: string[] = [];

  for (const workspace of workspaces) {
    const directory = join(root, workspace);
    const report = runBunAudit(directory);
    const lockfileModule = await import(join(directory, "bun.lock"));
    const lockfile = lockfileModule.default as BunLockfile;
    errors.push(...validateAudit(workspace, report, exceptions, lockfile));
  }

  if (errors.length > 0) {
    for (const error of errors) console.error(`frontend audit: ${error}`);
    process.exit(1);
  }
  console.log("frontend audit: all findings match reviewed exceptions");
}

if (import.meta.main) await main();
