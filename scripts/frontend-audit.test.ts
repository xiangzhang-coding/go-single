import { describe, expect, test } from "bun:test";

import type { AuditException, AuditReport, BunLockfile } from "./frontend-audit";
import { validateAudit } from "./frontend-audit";

const now = new Date("2026-08-17T00:00:00Z");
const lockfile: BunLockfile = {
  packages: {
    "@docusaurus/mdx-loader": [
      "@docusaurus/mdx-loader@3.10.2",
      "",
      { dependencies: { "image-size": "^2.0.2" } },
    ],
    "image-size": ["image-size@2.0.2", "", {}],
  },
};
const report: AuditReport = {
  "image-size": [
    { url: "https://github.com/advisories/GHSA-one", title: "one", severity: "high" },
    { url: "https://github.com/advisories/GHSA-two", title: "two", severity: "high" },
  ],
};
const exceptions: AuditException[] = ["GHSA-one", "GHSA-two"].map((advisory, index) => ({
  workspace: "website",
  package: "image-size",
  advisory,
  cve: `CVE-2025-1000${index}`,
  severity: "high",
  dependencyPath: [
    { package: "@docusaurus/mdx-loader", version: "3.10.2" },
    { package: "image-size", version: "2.0.2" },
  ],
  rationale: "Build-time only; no patched version.",
  owner: "maintainer",
  reviewedOn: "2026-08-17",
  expiresOn: "2026-11-17",
}));

describe("validateAudit", () => {
  test("accepts only exact findings and dependency paths", () => {
    expect(validateAudit("website", report, exceptions, lockfile, now)).toEqual([]);
  });

  test("rejects a new advisory", () => {
    const changed = structuredClone(report);
    changed["image-size"].push({
      url: "https://github.com/advisories/GHSA-new",
      title: "new",
      severity: "high",
    });
    expect(validateAudit("website", changed, exceptions, lockfile, now)).toContain(
      "website:image-size:GHSA-new: vulnerability is not allowed",
    );
  });

  test("rejects changed versions and stale exceptions", () => {
    const changedLockfile = structuredClone(lockfile);
    changedLockfile.packages["image-size"][0] = "image-size@2.0.3";
    expect(validateAudit("website", report, exceptions, changedLockfile, now).join("\n")).toContain(
      "lockfile does not contain image-size@2.0.2",
    );

    const missing = { "image-size": [report["image-size"][0]] };
    expect(validateAudit("website", missing, exceptions, lockfile, now).join("\n")).toContain(
      "stale exception; advisory was not reported",
    );
  });

  test("rejects expired exceptions", () => {
    expect(
      validateAudit("website", report, exceptions, lockfile, new Date("2026-11-18T00:00:00Z")).join("\n"),
    ).toContain("exception expired on 2026-11-17");
  });
});
