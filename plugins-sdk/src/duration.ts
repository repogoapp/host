/** A length of time as a plugin writes it: milliseconds, or a number and a unit (`"30s"`, `"5m"`, `"1h"`, `"7d"`). */
export type Duration = number | `${number}${"s" | "m" | "h" | "d"}`;

const unitMs = { s: 1_000, m: 60_000, h: 3_600_000, d: 86_400_000 } as const;

export function parseDuration(duration: Duration): number {
  if (typeof duration === "number") {
    if (!Number.isFinite(duration) || duration <= 0) throw new Error(`a duration is a positive number of ms: ${duration}`);
    return duration;
  }
  const match = /^(\d+(?:\.\d+)?)([smhd])$/.exec(duration);
  if (!match) throw new Error(`a duration is like "30s", "5m", "1h" or "7d": ${duration}`);
  const ms = Number(match[1]) * unitMs[match[2] as keyof typeof unitMs];
  if (ms <= 0) throw new Error(`a duration is longer than zero: ${duration}`);
  return ms;
}
