/**
 * Format a server ISO date-time for display. Falls back to the raw value when
 * the timestamp cannot be parsed, so we never render "Invalid Date".
 */
export function formatDateTime(value: string | undefined): string | undefined {
 if (!value) return undefined;
 const date = new Date(value);
 if (Number.isNaN(date.getTime())) return value;
 return date.toLocaleString('zh-CN', { dateStyle: 'medium', timeStyle: 'short' });
}
