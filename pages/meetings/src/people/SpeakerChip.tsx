// One speaker as the page shows it: a linked person opens their People page;
// a name the back end has not linked yet says so; an unnamed one stays muted.
import { href } from "../route";
import type { SpeakerView } from "./store";
import { shortName } from "./store";

export const STATE_NOTE: Record<string, string> = {
  unnamed: "unnamed",
  named: "no wiki page yet",
  resolving: "agent finding the wiki page…",
  linked: "",
};

export function SpeakerChip({ v, dot }: { v: SpeakerView; dot?: number }) {
  const dotEl = dot === undefined ? null : <span className="dot" data-c={dot} aria-hidden="true" />;
  if (v.state === "linked" && v.person)
    return (
      <a className="spk" data-state="linked" href={href(["people", v.person.slug])} title={v.person.description ?? v.person.name}>
        {dotEl}
        <span className="spk-name">{v.display}</span>
        {shortName(v.person) !== v.display ? <span className="spk-note">{shortName(v.person)}</span> : null}
      </a>
    );
  return (
    <span className="spk" data-state={v.state}>
      {dotEl}
      <span className="spk-name">{v.display}</span>
      <span className="spk-note">{STATE_NOTE[v.state]}</span>
    </span>
  );
}
