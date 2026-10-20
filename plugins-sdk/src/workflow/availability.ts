import { networkInterfaces } from "node:os";

export type Availability = { seen: number; offlineSince: number | null };
export type NetworkSource = () => boolean | null;

/** An assigned nonlocal interface detects known disconnection without external probes. */
export const networkAvailable: NetworkSource = () => {
  try {
    return Object.values(networkInterfaces()).flat().some(address => address && !address.internal &&
      (address.family === "IPv4" ? !address.address.startsWith("169.254.") : !address.address.startsWith("fe80:")));
  } catch { return null; }
};

export function nextAvailability(saved: Availability | null, now: number, online: boolean | null): Availability {
  const offlineSince = online === true ? null : online === false ? saved?.offlineSince ?? now : saved?.offlineSince ?? null;
  return { seen: now, offlineSince };
}
