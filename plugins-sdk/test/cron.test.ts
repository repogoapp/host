import { expect, test } from "bun:test";
import { nextDue, parseSchedule } from "../src/cron";

const ny = "America/New_York";
const utc = "UTC";
const at = (iso: string) => Date.parse(iso);
const iso = (ms: number | null) => (ms === null ? null : new Date(ms).toISOString());

test("fields read as cron reads them", () => {
  const s = parseSchedule("*/15 9-17 * * 1-5");
  expect([...s.minutes]).toEqual([0, 15, 30, 45]);
  expect([...s.hours]).toEqual([9, 10, 11, 12, 13, 14, 15, 16, 17]);
  expect([...s.weekdays]).toEqual([1, 2, 3, 4, 5]);
  expect(s.seconds).toBeNull();
  expect([...parseSchedule("*/5 * * * * *").seconds!]).toEqual([0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55]);
  expect([...parseSchedule("0,30,10-50/20 * * * *").minutes]).toEqual([0, 30, 10, 50]);
  expect([...parseSchedule("0 0 * * 7").weekdays]).toEqual([0]);
  expect([...parseSchedule("5/20 * * * *").minutes]).toEqual([5, 25, 45]);
});

test("a bad schedule says what's wrong", () => {
  for (const [source, says] of [
    ["* * * *", "has 4 fields"],
    ["60 * * * *", "outside 0-59"],
    ["* 24 * * *", "outside 0-23"],
    ["*/0 * * * *", "at least 1"],
    ["a * * * *", "isn't a whole number"],
    ["5-1 * * * *", "outside"],
    ["* * * * * * *", "has 7 fields"],
  ] as const) {
    expect(() => parseSchedule(source)).toThrow(says);
  }
});

test("the next due time, strictly after", () => {
  const cases: [string, string, string, string][] = [
    // schedule, zone, after, due
    ["*/5 * * * *", utc, "2026-10-17T10:02:30Z", "2026-10-17T10:05:00.000Z"],
    ["*/5 * * * *", utc, "2026-10-17T10:05:00Z", "2026-10-17T10:10:00.000Z"],
    ["*/5 * * * * *", utc, "2026-10-17T10:02:03Z", "2026-10-17T10:02:05.000Z"],
    ["*/5 * * * * *", utc, "2026-10-17T10:02:58Z", "2026-10-17T10:03:00.000Z"],
    ["0 * * * *", utc, "2026-10-17T10:00:00Z", "2026-10-17T11:00:00.000Z"],
    // Every 5 hours restarts each day: 20:00, then 00:00, not 01:00.
    ["0 */5 * * *", utc, "2026-10-17T20:00:00Z", "2026-10-18T00:00:00.000Z"],
    // Every 25 minutes restarts each hour: :50, then :00.
    ["*/25 * * * *", utc, "2026-10-17T10:50:00Z", "2026-10-17T11:00:00.000Z"],
    ["0 9 * * 1-5", ny, "2026-10-17T12:00:00Z", "2026-10-19T13:00:00.000Z"],
    // Both day fields set: either matches (the 1st, or any Monday).
    ["0 0 1 * 1", utc, "2026-10-17T00:00:00Z", "2026-10-19T00:00:00.000Z"],
    ["0 0 29 2 *", utc, "2026-03-01T00:00:00Z", "2028-02-29T00:00:00.000Z"],
    // Spring forward: 2:30 doesn't exist on 03-08, so the next is 03-09.
    ["30 2 * * *", ny, "2026-03-07T08:00:00Z", "2026-03-09T06:30:00.000Z"],
    // Fall back: 1:30 happens twice on 11-01; only the first is due.
    ["30 1 * * *", ny, "2026-11-01T05:00:00Z", "2026-11-01T05:30:00.000Z"],
    ["30 1 * * *", ny, "2026-11-01T05:30:00Z", "2026-11-02T06:30:00.000Z"],
  ];
  for (const [schedule, zone, after, due] of cases) {
    expect({ schedule, after, due: iso(nextDue(parseSchedule(schedule), at(after), zone)) }).toEqual({ schedule, after, due });
  }
});

test("a schedule that's never due has no next time", () => {
  expect(nextDue(parseSchedule("0 0 31 2 *"), at("2026-01-01T00:00:00Z"), utc)).toBeNull();
});
