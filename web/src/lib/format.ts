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

/** Human-readable byte size, for an attachment's `byte_size`. */
export function formatBytes(value: number | undefined): string | undefined {
 if (value === undefined || !Number.isFinite(value) || value < 0) return undefined;
 if (value < 1024) return `${value} B`;
 if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
 return `${(value / (1024 * 1024)).toFixed(2)} MiB`;
}
