import { afterEach, describe, expect, it, vi } from "vitest";
import { fakeRegistry, loaded, SLUG } from "./fake-registry";
import type { Entry } from "./registry";
import type { LabelsRecord, PeopleIndexRecord, Rec } from "./types";

afterEach(() => vi.unstubAllGlobals());

async function fresh(pathname = `/a/${SLUG}/`) {
  vi.resetModules();
  vi.stubGlobal("location", { pathname, search: "" });
  return import("./store");
}

describe("derive", () => {
  it("splits types from the open list, marks queued rows, fills a missing id and state, and drops a malformed index", async () => {
    const { derive } = await fresh();
    const recs = [
      { key: "rec.wx42", version: 3, value: { schema: "meeting@1", data: { id: "wx42", state: "aligned", labels: ["露点", "巡检"] } } },
      { key: "rec.wx43", version: 1, value: { schema: "meeting@1", data: {} } },
    ] as Entry<Rec>[];
    const labels = {
      closed: [
        { name: "人:Alice Example", kind: "person" },
        { name: " ", kind: "type" },
        { name: "巡检", kind: "type" },
      ],
      questions: "none",
    } as unknown as LabelsRecord;
    const d = derive(recs, [{ key: "q.wx43", version: 1 }], labels, { people: "none" } as unknown as PeopleIndexRecord);
    expect(d.rows.map((r) => [r.rec.id, r.rec.state, r.version, r.queued])).toEqual([
      ["wx42", "aligned", 3, false],
      ["wx43", "unknown", 1, true],
    ]);
    expect(d.vocabulary.closed).toEqual(["巡检"]);
    expect(d.vocabulary.open.sort()).toEqual(["露点", "人:Alice Example"].sort());
    expect(d.closed.map((c) => c.name)).toEqual(["人:Alice Example", "巡检"]);
    expect(d.questions).toEqual([]);
    expect(d.peopleIndex).toBeNull();
  });
});

describe("load", () => {
  it("reads records, the queue, the questions and the people index in one pass", async () => {
    fakeRegistry({
      "rec.wx44": { id: "wx44", state: "uploaded", labels: ["雨量筒"] },
      "q.wx44": { state: "uploaded" },
      labels: { closed: [{ name: "补测", kind: "type" }], questions: [{ id: "qid-rue", status: "open" }] },
      "people.index": { people: [{ slug: "alice", name: "Alice Example" }] },
    });
    const st = (await loaded()).getState();
    expect([st.loaded, st.loadError, st.contributor]).toEqual([true, null, false]);
    expect(st.rows.map((r) => [r.rec.id, r.queued])).toEqual([["wx44", true]]);
    expect(st.vocabulary).toEqual({ closed: ["补测"], open: ["雨量筒"] });
    expect(st.questions.map((q) => q.id)).toEqual(["qid-rue"]);
    expect(st.peopleIndex?.people.map((p) => p.slug)).toEqual(["alice"]);
  });

  it("takes a valid config zone, ignores an unknown one, and stops following config once the viewer picks", async () => {
    const fake = fakeRegistry({ config: { tz: "Mars/Olympus" } });
    const s = await loaded();
    const before = s.getState().tz;
    expect(before).not.toBe("Mars/Olympus");
    fake.store.get("config")!.value.data = { tz: "Pacific/Chatham" };
    await s.load();
    expect(s.getState().tz).toBe("Pacific/Chatham");
    s.setTz("Europe/Lisbon");
    await s.load();
    expect(s.getState().tz).toBe("Europe/Lisbon");
  });

  it("asks for the /a/ address when the page has no slug", async () => {
    const s = await fresh("/");
    await s.load();
    expect(s.getState().loadError).toMatch(/through its \/a\/<slug> address/);
  });

  it("loads an empty list for a contributor link whose list is refused", async () => {
    const refuse = (status: number, code: string) => new Response(JSON.stringify({ error: code, code }), { status });
    vi.stubGlobal("fetch", vi.fn(async () => refuse(403, "role_insufficient")));
    const s = await fresh();
    await s.load();
    expect(s.getState()).toMatchObject({ contributor: true, loaded: true, loadError: null, rows: [] });
  });

  it("puts any other failure into words as the load error", async () => {
    const answers: (() => Promise<Response>)[] = [
      async () => new Response(JSON.stringify({ error: "bad", code: "token_invalid" }), { status: 401 }),
      async () => {
        throw new TypeError("offline");
      },
    ];
    const got: (string | null)[] = [];
    for (const answer of answers) {
      vi.stubGlobal("fetch", vi.fn(answer));
      const s = await fresh();
      await s.load();
      expect(s.getState().loaded).toBe(false);
      got.push(s.getState().loadError);
    }
    expect(got[0]).toMatch(/^Could not read the recordings: .*no access to the page/);
    expect(got[1]).toMatch(/^Could not read the recordings: Network error: offline/);
  });
});
