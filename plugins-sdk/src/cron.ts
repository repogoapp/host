// A cron schedule: five fields (minute hour day-of-month month day-of-week), or
// six with seconds first. Each field is `*`, a number, a range `1-5`, a list
// `0,30`, or a step `*/5` or `10-50/10`. Day of week is 0-7, Sunday 0 or 7.
// As in cron, when both day fields are restricted, either one matching is enough.

export type Schedule = {
  seconds: Set<number> | null;
  minutes: Set<number>;
  hours: Set<number>;
  days: Set<number>;
  months: Set<number>;
  weekdays: Set<number>;
  /** Day fields given as other than `*`, for cron's either-day rule. */
  daysRestricted: boolean;
  weekdaysRestricted: boolean;
};

const ranges = { second: [0, 59], minute: [0, 59], hour: [0, 23], day: [1, 31], month: [1, 12], weekday: [0, 7] } as const;

/** Reads a schedule, or throws saying what's wrong with it. */
export function parseSchedule(source: string): Schedule {
  const fields = source.trim().split(/\s+/);
  if (fields.length !== 5 && fields.length !== 6) {
    throw new Error(`"${source}" has ${fields.length} fields; a cron has 5 (minute hour day month weekday), or 6 with seconds first`);
  }
  const [second, minute, hour, day, month, weekday] = fields.length === 6 ? fields : [null, ...fields];
  const weekdays = field(weekday!, "weekday");
  // Sunday is 0 or 7.
  if (weekdays.has(7)) { weekdays.delete(7); weekdays.add(0); }
  return {
    seconds: second === null ? null : field(second, "second"),
    minutes: field(minute!, "minute"),
    hours: field(hour!, "hour"),
    days: field(day!, "day"),
    months: field(month!, "month"),
    weekdays,
    daysRestricted: day !== "*",
    weekdaysRestricted: weekday !== "*",
  };
}

function field(source: string, name: keyof typeof ranges): Set<number> {
  const [min, max] = ranges[name];
  const out = new Set<number>();
  for (const part of source.split(",")) {
    const [span, stepText] = part.split("/");
    const step = stepText === undefined ? 1 : whole(stepText, name);
    if (step < 1) throw new Error(`the ${name} step in "${part}" must be at least 1`);
    let [from, to] = [min, max] as number[];
    if (span !== "*") {
      const [a, b] = span.split("-");
      from = whole(a, name);
      to = b === undefined ? (stepText === undefined ? from : max) : whole(b, name);
    }
    if (from < min || to > max || from > to) throw new Error(`the ${name} "${part}" is outside ${min}-${max}`);
    for (let v = from; v <= to; v += step) out.add(v);
  }
  return out;
}

function whole(text: string, name: string) {
  if (!/^\d+$/.test(text)) throw new Error(`the ${name} "${text}" isn't a whole number`);
  return Number(text);
}

/** Throws unless `zone` is a time zone this machine knows, such as "America/New_York". */
export function checkTimezone(zone: string) {
  new Intl.DateTimeFormat("en-US", { timeZone: zone });
}

type Wall = { year: number; month: number; day: number; weekday: number; hour: number; minute: number; second: number };

const formats = new Map<string, Intl.DateTimeFormat>();

/** The wall clock in `zone` at `ms`. */
function wall(ms: number, zone: string): Wall {
  let format = formats.get(zone);
  if (!format) {
    format = new Intl.DateTimeFormat("en-US", {
      timeZone: zone, hourCycle: "h23", weekday: "short",
      year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric", second: "numeric",
    });
    formats.set(zone, format);
  }
  const parts: Record<string, string> = {};
  for (const p of format.formatToParts(new Date(ms))) parts[p.type] = p.value;
  return {
    year: Number(parts.year), month: Number(parts.month), day: Number(parts.day),
    weekday: ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].indexOf(parts.weekday),
    hour: Number(parts.hour), minute: Number(parts.minute), second: Number(parts.second),
  };
}

const minute = 60_000;
/** How far ahead a due time is looked for: past a leap day, with room. */
const horizon = 4 * 366 * 24 * 60 * minute;

/**
 * The first due time strictly after `after` (ms), in `zone`, or null when there
 * is none within four years (`0 0 31 2 *`). A wall time that a fall-back night
 * shows twice is due only the first time.
 */
export function nextDue(s: Schedule, after: number, zone: string): number | null {
  // Whole minutes in UTC are whole minutes in every zone in use.
  let at = Math.floor(after / minute) * minute;
  const end = after + horizon;
  // The seconds of the minute holding `after` that are still to come.
  if (s.seconds) {
    const first = firstSecond(s, at, after, zone);
    if (first !== null) return first;
  }
  at += minute;
  while (at < end) {
    const w = wall(at, zone);
    if (!s.months.has(w.month)) { at += untilNextDay(w); continue; }
    if (!dayMatches(s, w)) { at += untilNextDay(w); continue; }
    if (!s.hours.has(w.hour)) { at += (60 - w.minute) * minute; continue; }
    if (!s.minutes.has(w.minute) || repeated(at, w, zone)) { at += minute; continue; }
    if (!s.seconds) return at;
    const first = firstSecond(s, at, at - 1, zone);
    if (first !== null) return first;
    at += minute;
  }
  return null;
}

/** In the minute starting at `start`, the first listed second after `after`, when the minute itself is due. */
function firstSecond(s: Schedule, start: number, after: number, zone: string) {
  const w = wall(start, zone);
  if (!s.months.has(w.month) || !dayMatches(s, w) || !s.hours.has(w.hour) || !s.minutes.has(w.minute) || repeated(start, w, zone)) return null;
  for (let second = 0; second < 60; second++) {
    const at = start + second * 1000;
    if (s.seconds!.has(second) && at > after) return at;
  }
  return null;
}

function dayMatches(s: Schedule, w: Wall) {
  const day = s.days.has(w.day), weekday = s.weekdays.has(w.weekday);
  if (s.daysRestricted && s.weekdaysRestricted) return day || weekday;
  return day && weekday;
}

/** Minutes to the next local midnight, near enough: the loop checks again when it lands. */
function untilNextDay(w: Wall) {
  return ((23 - w.hour) * 60 + (60 - w.minute)) * minute;
}

/** True when the same wall minute already happened within the last two hours (a fall-back night). */
function repeated(at: number, w: Wall, zone: string) {
  for (const back of [60, 120]) {
    const earlier = wall(at - back * minute, zone);
    if (earlier.day === w.day && earlier.hour === w.hour && earlier.minute === w.minute) return true;
  }
  return false;
}
