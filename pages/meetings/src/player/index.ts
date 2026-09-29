// The recording player, as one component.
//
//   <UnifiedPlayer rec={rec} personHref={(p) => href(["people", p.slug])} />
//
// Other views seek it through the bus — no provider needed:
//   const seek = usePlayerSeek();  seek(t)
//   const { time, playing, rate } = usePlayerState();
//   <TimeButton t={s} />   a timestamp that moves the player
import { seek as busSeek, useBus } from "./bus";
export { UnifiedPlayer, type UnifiedPlayerProps } from "./UnifiedPlayer";
export { SpeakerName } from "./SpeakerName";
export { RATES, type Rate } from "./bus";
export { TimeButton, PlayerProvider, usePlayer } from "./compat";

export function usePlayerSeek(): (t: number) => void {
  return busSeek;
}

export function usePlayerState(): { time: number; playing: boolean; rate: number; duration: number } {
  const b = useBus();
  return { time: b.time, playing: b.playing, rate: b.rate, duration: b.duration };
}

export const seekPlayer = busSeek;
