"use client";

import { cn } from "@/lib/utils";

function resolveMeterFill(value: number, warnAt: number, badAt: number, color?: string): string {
  if (color) return color;
  if (value >= badAt) return "var(--ma-bad, #ff5a52)";
  if (value >= warnAt) return "var(--ma-warn, #f2a93b)";
  return "var(--ma-fg, var(--foreground))";
}

/**
 * MeshAtlas-inspired 20/30-segment discrete HUD meter.
 * Automatically shifts active bar fill to amber / coral-red above warn / bad thresholds.
 */
export function SegmentedMeter({
  value,
  segments = 24,
  warnAt = 75,
  badAt = 90,
  color,
  className,
}: {
  /** Percentage 0–100 */
  value: number;
  segments?: number;
  warnAt?: number;
  badAt?: number;
  /** Optional explicit fill override (e.g. semantic category tint) */
  color?: string;
  className?: string;
}) {
  const clamped = Math.max(0, Math.min(100, Number.isFinite(value) ? value : 0));
  const filled = Math.round((clamped / 100) * segments);
  const autoFill = resolveMeterFill(clamped, warnAt, badAt, color);

  return (
    <div
      data-slot="segmented-meter"
      className={cn("flex h-2.5 w-full items-stretch gap-[2px]", className)}
      role="progressbar"
      aria-valuenow={Math.round(clamped)}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      {Array.from({ length: segments }, (_, i) => {
        const active = i < filled;
        return (
          <span
            // biome-ignore lint/suspicious/noArrayIndexKey: fixed-length meter segments
            key={i}
            className="flex-1 transition-colors"
            style={{
              backgroundColor: active ? autoFill : "var(--ma-seg-empty, var(--muted))",
            }}
          />
        );
      })}
    </div>
  );
}

/**
 * MeshAtlas terminal blinking block cursor (8px × 14px).
 */
export function BlinkingCursor({ className }: { className?: string }) {
  return (
    <span
      aria-hidden
      data-slot="hud-cursor"
      className={cn(
        "inline-block h-[13px] w-[7px] bg-foreground align-middle [animation:ma-blink_1s_steps(1)_infinite]",
        className,
      )}
    />
  );
}

/**
 * Lightweight pure-SVG 64×16 sparkline matching MeshAtlas node index telemetry.
 */
export function MiniSparkline({
  values,
  width = 64,
  height = 16,
  stroke = "currentColor",
  className,
}: {
  values: number[];
  width?: number;
  height?: number;
  stroke?: string;
  className?: string;
}) {
  if (values.length < 2) return null;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const pad = 1.5;
  const points = values
    .map((v, i) => {
      const x = (i / (values.length - 1)) * width;
      const y = height - pad - ((v - min) / span) * (height - pad * 2);
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={cn("shrink-0 overflow-visible text-muted-foreground", className)}
      aria-hidden="true"
    >
      <title>Sparkline</title>
      <polyline fill="none" stroke={stroke} strokeWidth={1.25} points={points} />
    </svg>
  );
}

/**
 * MeshAtlas 4-tier proxy latency color palette:
 *   < 80ms   -> #3ecf8e
 *   80–160ms -> #e6c34a
 *   160–260ms-> #f29a3b
 *   >= 260ms -> #ff6b6b
 */
export function meshAtlasLatencyColor(ms: number): string {
  if (ms <= 0) return "#ff5a52";
  if (ms < 80) return "#3ecf8e";
  if (ms < 160) return "#e6c34a";
  if (ms < 260) return "#f29a3b";
  return "#ff6b6b";
}
