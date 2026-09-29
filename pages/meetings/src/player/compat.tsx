// The player API Detail and Alignment use, served by the bus.
import type { ReactNode } from "react";
import { fmtPos } from "../time";
import { seek, useBus } from "./bus";

/** Kept so `<PlayerProvider>` wrappers still compile; the bus needs no provider. */
export function PlayerProvider({ children }: { children: ReactNode }) {
  return <>{children}</>;
}

export function usePlayer(): { time: number; playing: boolean; seek: (t: number) => void } {
  const b = useBus();
  return { time: b.time, playing: b.playing, seek };
}

export function TimeButton({ t, at }: { t: number; at?: number }) {
  return (
    <button type="button" className="ts" onClick={() => seek(at ?? t)} aria-label={`Move the player to ${fmtPos(t)}`}>
      {fmtPos(t)}
    </button>
  );
}
