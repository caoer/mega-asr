// The summaries' markdown: headings, nested "-" lists, **bold**, `code`,
// paragraphs. Rendered as elements, never as HTML strings.
import type { ReactNode } from "react";

function inline(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  const re = /\*\*(.+?)\*\*|`([^`]+)`/g;
  let at = 0;
  let m: RegExpExecArray | null;
  let i = 0;
  while ((m = re.exec(text))) {
    if (m.index > at) out.push(text.slice(at, m.index));
    out.push(m[1] !== undefined ? <strong key={i++}>{m[1]}</strong> : <code key={i++}>{m[2]}</code>);
    at = m.index + m[0].length;
  }
  if (at < text.length) out.push(text.slice(at));
  return out;
}

interface Item {
  text: string;
  kids: Item[];
}

export function Markdown({ source }: { source: string }) {
  const blocks: ReactNode[] = [];
  const lines = source.replace(/\r/g, "").split("\n");
  let i = 0;
  let k = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) {
      blocks.push(<h4 key={k++}>{inline(h[2])}</h4>);
      i++;
      continue;
    }
    if (/^\s*[-*]\s+/.test(line)) {
      const root: Item = { text: "", kids: [] };
      const stack: { indent: number; item: Item }[] = [{ indent: -1, item: root }];
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        const m = /^(\s*)[-*]\s+(.*)$/.exec(lines[i])!;
        const indent = m[1].replace(/\t/g, "    ").length;
        while (stack.length > 1 && stack[stack.length - 1].indent >= indent) stack.pop();
        const item: Item = { text: m[2], kids: [] };
        stack[stack.length - 1].item.kids.push(item);
        stack.push({ indent, item });
        i++;
      }
      const render = (items: Item[]): ReactNode => (
        <ul>
          {items.map((it, j) => (
            <li key={j}>
              {inline(it.text)}
              {it.kids.length ? render(it.kids) : null}
            </li>
          ))}
        </ul>
      );
      blocks.push(<div key={k++}>{render(root.kids)}</div>);
      continue;
    }
    const para: string[] = [];
    while (i < lines.length && lines[i].trim() && !/^\s*[-*]\s+/.test(lines[i]) && !/^#{1,6}\s/.test(lines[i])) para.push(lines[i++].trim());
    blocks.push(<p key={k++}>{inline(para.join(" "))}</p>);
  }
  return <div className="md">{blocks}</div>;
}
