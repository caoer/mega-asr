import { afterEach, describe, expect, it, vi } from "vitest";
import { fakeRegistry, SLUG, type Fake } from "./fake-registry";
import type { LabelQuestion, LabelsRecord, Rec } from "./types";

afterEach(() => vi.unstubAllGlobals());

async function questions() {
  vi.resetModules();
  vi.stubGlobal("location", { pathname: `/a/${SLUG}/`, search: "" });
  return import("./questions");
}

describe("phase", () => {
  const cases: [LabelQuestion, string][] = [
    [{ id: "qid-ash", status: "expired" }, "expired"],
    [{ id: "qid-ash", status: "approved", error: "rename: no such label" }, "failed"],
    [{ id: "qid-ash", status: "declined" }, "declined"],
    [{ id: "qid-ash", status: "approved" }, "applying"],
    [{ id: "qid-ash" }, "expired"],
    [{ id: "qid-ash", status: "applied" }, "applied"],
    [{ id: "qid-ash", status: "open" }, "open"],
  ];
  it.each(cases)("reads %o as %s", async (q, want) => {
    const { phase } = await questions();
    expect(phase(q)).toBe(want);
  });
});

describe("proposalText", () => {
  const cases: [string[] | undefined, string][] = [
    [["split", "风速计", "rec.wx44", "rec.wx42", "rec.wx43"], "Split “风速计” across 3 recordings, each getting its own labels."],
    [["merge", "风速计", "雨量筒", "百叶箱", "--into", "器材"], "Merge “风速计”, “雨量筒”, “百叶箱” into “器材” on every recording."],
    [["archive", "巡检"], "archive 巡检"],
    [["set", "rec.wx43", "补测", "露点"], "Set the labels of one recording to “补测”, “露点”."],
    [["promote", "人:Alice Example", "person"], "Add “人:Alice Example” to the closed list as a person."],
    [[], "A question with no change attached."],
    [["rename", "百叶箱", "遮阳罩"], "Rename “百叶箱” to “遮阳罩” on every recording."],
    [["demote", "人:Alice Example"], "Take “人:Alice Example” off the closed list; recordings keep it as an open label."],
    [["set", "rec.wx42"], "Set the labels of one recording to none."],
    [["promote", "露点", "shelter"], "Add “露点” to the closed list as shelter."],
    [["split", "露点", "rec.wx41"], "Split “露点” across 1 recording, each getting its own labels."],
  ];
  it.each(cases)("words %j", async (p, want) => {
    const { proposalText } = await questions();
    expect(proposalText(p)).toBe(want);
  });
});

describe("affected", () => {
  const recs = [
    { id: "wx46", state: "deleted", labels: ["风速计", "雨量筒"] },
    { id: "wx41", state: "ingested", labels: [] },
    { id: "wx44", state: "failed", labels: ["雨量筒", "风速计"] },
    { id: "wx42", state: "ingested", labels: ["百叶箱"] },
    { id: "wx43", state: "uploaded", labels: ["风速计"] },
  ] as Rec[];
  const ids = async (p: string[]) => (await questions()).affected(p, recs).map((r) => r.id);

  it("finds each live recording carrying any merged label once, never a deleted one", async () => {
    expect(await ids(["merge", "风速计", "雨量筒", "--into", "器材"])).toEqual(["wx44", "wx43"]);
  });
  it("finds the carriers of a renamed, demoted, promoted or split label", async () => {
    for (const op of ["rename", "demote", "promote", "split"]) expect(await ids([op, "百叶箱", "x"])).toEqual(["wx42"]);
  });
  it("finds one recording for set, by id with or without the rec. prefix, labelled or not", async () => {
    expect(await ids(["set", "rec.wx41", "巡检"])).toEqual(["wx41"]);
    expect(await ids(["set", "wx46"])).toEqual([]);
  });
  it("finds nothing for an empty or unknown proposal", async () => {
    expect(await ids([])).toEqual([]);
    expect(await ids(["archive", "风速计"])).toEqual([]);
  });
});

describe("withAnswer", () => {
  // withAnswer reads no proposal, so these questions carry only text.
  const record = (): LabelsRecord & Record<string, unknown> & { questions: LabelQuestion[] } => ({
    closed: [{ name: "巡检", kind: "type" }],
    questions: [
      { id: "qid-fir", status: "open", text: "Keep 露点?" },
      { id: "qid-bay", status: "open", text: "Keep 百叶箱?", ask: "page" },
      { id: "qid-yew", status: "applied", answer: "Approve (meetings page)" },
    ],
    note: { from: "a test" },
  });

  it("answers the named question alone, drops the milliseconds, and keeps every other field", async () => {
    const { withAnswer } = await questions();
    const before = record();
    const after = withAnswer(before, "qid-bay", "approved", "Approve (meetings page)", new Date("2020-02-19T22:05:41.987Z"));
    expect(after.questions).toEqual([
      before.questions[0],
      { ...before.questions[1], status: "approved", answer: "Approve (meetings page)", answered: "2020-02-19T22:05:41Z" },
      before.questions[2],
    ]);
    expect(after).toMatchObject({ closed: before.closed, note: before.note, updated_at: "2020-02-19T22:05:41Z", updated_by: "meetings page" });
  });

  it("refuses a question that is gone or no longer open", async () => {
    const { withAnswer, NotOpen } = await questions();
    for (const id of ["qid-yew", "qid-elm"]) expect(() => withAnswer(record(), id, "declined", "", new Date())).toThrow(NotOpen);
  });
});

describe("answerQuestion", () => {
  const at = () => new Date("2020-02-20T03:11:07.250Z");
  const seed = () => ({ labels: { questions: [{ id: "qid-oak", status: "open", text: "Keep 补测?" }] } });

  /** The fake behind a fetch that loses the next `lose` label writes and answers the inbox with `ring`. */
  function around(fake: Fake, lose: number, ring: number) {
    const inner = globalThis.fetch;
    const rings: Record<string, string>[] = [];
    vi.stubGlobal("fetch", async (url: string, init?: RequestInit) => {
      const path = new URL(url, "https://pages.example").pathname;
      if (path === `/i/${SLUG}`) {
        rings.push(init?.headers as Record<string, string>);
        return new Response(JSON.stringify(ring === 200 ? { seq: 4 } : { error: "gone", code: "no_such_inbox" }), { status: ring });
      }
      if (init?.method === "PUT" && path.endsWith("/labels") && lose > 0) {
        lose--;
        fake.store.get("labels")!.version++; // the label agent wrote first
      }
      return inner(url, init);
    });
    return rings;
  }

  it("retries lost writes, logs the answer, and rings the inbox once", async () => {
    const fake = fakeRegistry(seed());
    const rings = around(fake, 2, 200);
    const { answerQuestion } = await questions();
    expect(await answerQuestion("qid-oak", "approved", at)).toEqual({ rang: true });
    expect(fake.writes[0]).toBe("PUT labels v=3");
    expect(fake.data("labels")?.questions).toEqual([{ id: "qid-oak", status: "approved", text: "Keep 补测?", answer: "Approve (meetings page)", answered: "2020-02-20T03:11:07Z" }]);
    const log = [...fake.store.entries()].find(([k]) => k.startsWith("labellog."));
    expect(log?.[0]).toMatch(/^labellog\.20200220T031107\.250000Z\.\d{4}\.[0-9a-f]{6}$/);
    expect(log?.[1].value).toMatchObject({ schema: "labellog@1", data: { by: "meetings page", op: ["question", "qid-oak", "approved"], question: "qid-oak" } });
    expect(rings).toEqual([expect.objectContaining({ "x-ccc-auth": "cookie", "x-inbox-event": "human.message", "x-inbox-delivery": "labels-qid-oak-approved" })]);
  });

  it("keeps a decline that could not ring", async () => {
    const fake = fakeRegistry(seed());
    around(fake, 0, 404);
    const { answerQuestion } = await questions();
    expect(await answerQuestion("qid-oak", "declined", at)).toEqual({ rang: false });
    expect((fake.data("labels")?.questions as LabelQuestion[])[0].status).toBe("declined");
  });

  it("gives up with the conflict after five lost writes", async () => {
    const fake = fakeRegistry(seed());
    const rings = around(fake, 5, 200);
    const { answerQuestion } = await questions();
    await expect(answerQuestion("qid-oak", "approved", at)).rejects.toMatchObject({ code: "version_conflict" });
    expect(fake.writes).toEqual([]);
    expect(rings).toEqual([]);
  });
});
