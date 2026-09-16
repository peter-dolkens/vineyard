/**
 * Version comparison for daemon/extension updates. Only the numeric core (major.minor.patch) counts;
 * anything after it ("-dev", "-3-gabc123", "+build") is ignored, so two different dev builds of the same
 * release never fight over which one is "newer". Unparseable versions ("dev", "") never trigger updates.
 */

export interface ParsedVersion {
  major: number;
  minor: number;
  patch: number;
  /** Everything after the numeric core, without the leading separator. */
  suffix: string;
}

export function parseVersion(v: string | undefined): ParsedVersion | undefined {
  if (!v) return undefined;
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:[-+](.*))?$/.exec(v.trim());
  if (!m) return undefined;
  return { major: Number(m[1]), minor: Number(m[2]), patch: Number(m[3]), suffix: m[4] ?? '' };
}

/** Negative if a < b, positive if a > b, 0 if their numeric cores match. Unparseable sorts as equal. */
export function compareVersions(a: string | undefined, b: string | undefined): number {
  const pa = parseVersion(a);
  const pb = parseVersion(b);
  if (!pa || !pb) return 0;
  return pa.major - pb.major || pa.minor - pb.minor || pa.patch - pb.patch;
}

/** True only when both parse and candidate's numeric core is strictly greater. */
export function isNewer(candidate: string | undefined, current: string | undefined): boolean {
  if (!parseVersion(candidate) || !parseVersion(current)) return false;
  return compareVersions(candidate, current) > 0;
}
