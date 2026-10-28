import { describe, expect, it } from "vitest";
import { withNames } from "./store";
import { nameParts, turnsOf } from "./player/turns";
import type { NamesChange, Segment } from "./types";

// The people are made up.
const seg = (idx: number, text: string, start = idx): Segment => ({ speaker: "S1", start, end: start + 1, text, raw: null, idx });

describe("the names pass on the page", () => {
  const changes: NamesChange[] = [
    { src: "ours", i: 0, before: "我跟小西说", text: "我跟小溪说", names: [{ text: "小溪", person: "lin-xiaoxi", heard: "小西" }] },
    { src: "feishu", i: 1, before: "明元在", text: "明远在", names: [{ text: "明远", person: "zhou-mingyuan", heard: "明元" }] },
    { src: "feishu", i: 2, before: "text Feishu has since changed", text: "x", names: [] },
  ];

  it("gives our corrected segments their names and lays Feishu's corrections over its turns", () => {
    const ours = withNames([seg(0, "我跟小溪说")], changes, "ours");
    expect(ours[0].names).toEqual(changes[0].names);
    const feishu = withNames([seg(0, "你好"), seg(1, "明元在"), seg(2, "something else")], changes, "feishu");
    expect(feishu.map((s) => s.text)).toEqual(["你好", "明远在", "something else"]);
    expect(feishu[1].raw).toBe("明元在");
  });

  it("links repaired names with what was heard, and known full names", () => {
    const turn = turnsOf(withNames([seg(0, "我跟小溪说"), seg(1, "周明远也来了")], changes, "ours"))[0];
    const parts = nameParts(turn.text, turn.names, new Map([["周明远", "zhou-mingyuan"]]));
    expect(parts).toEqual([
      { t: "我跟" },
      { t: "小溪", person: "lin-xiaoxi", heard: "小西" },
      { t: "说" },
      { t: "周明远", person: "zhou-mingyuan" },
      { t: "也来了" },
    ]);
  });
});
