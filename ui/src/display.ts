export const EVENT_SIGNATURE_MAX_LENGTH = 72;

export function middleEllipsis(value: string, maximum = EVENT_SIGNATURE_MAX_LENGTH) {
  if (value.length <= maximum) return value;
  const marker = "…";
  const ending = Math.max(16, Math.floor(maximum * 0.3));
  const beginning = maximum - ending - marker.length;
  return `${value.slice(0, beginning)}${marker}${value.slice(-ending)}`;
}

export function displayValue(value: unknown) {
  return typeof value === "string" ? value : JSON.stringify(value);
}
