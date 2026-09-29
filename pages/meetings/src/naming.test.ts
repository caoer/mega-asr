import { afterEach, describe, expect, it, vi } from "vitest";
import type { PeopleIndexRecord, Speaker } from "./types";
import { fakeRegistry, loaded, SLUG } from "./fake-registry";

async function people() {
  vi.resetModules();
  vi.stubGlobal("location", { pathname: `/a/${SLUG}/`, search: "" });
  return import("./people/store");
}

afterEach(() => vi.unstubAllGlobals());

// Two people. The index holds Alice, under a name with a trailing gloss and
// one alias. Bob has a page too, but the index does not list it: he links only
// where a record carries his `person`, and elsewhere waits for the drain.
const alice = { slug: "alice", name: "Alice Example (QA)", aliases: ["Alice E."], wiki_url: "", pages: [] };
const bobLinked: Speaker = { name: "Bob Example", person: "bob" };

describe("withName", () => {
  it("links a person only on an exact name, short name or alias, ignoring case and spacing", async () => {
    const { withName } = await people();
    const cases: [string, string | undefined][] = [
      ["Alice", undefined],
      ["ALICE e.", "alice"],
      ["Alice Example (QA)", "alice"],
      [" alice   example  ", "alice"],
      ["Bob Example", undefined],
      ["Alice E. Jr", undefined],
    ];
    for (const [typed, person] of cases) {
      const [entry] = withName([{ name: "Speaker 4" }], "Speaker 4", typed, [alice]);
      expect(entry).toEqual({ role: "Speaker 4", name: typed.trim(), ...(person ? { person } : {}) });
    }
  });

  it("renames one entry alone, keeping its first role and any unknown field, and drops a person the new name does not match", async () => {
    const { withName } = await people();
    const before: Speaker[] = [bobLinked, { role: "Speaker 0", name: "Alice Example", person: "alice", seen: 3 } as Speaker, { name: "Speaker 1" }];
    const after = withName(before, "Speaker 0", "Bob Example", [alice]);
    expect(after).toEqual([bobLinked, { role: "Speaker 0", name: "Bob Example", seen: 3 }, { name: "Speaker 1" }]);
    expect(after[0]).toBe(before[0]);
    expect(after[2]).toBe(before[2]);
  });

  it("appends an entry for a label the record lacks", async () => {
    const { withName } = await people();
    expect(withName([], "Speaker 7", "Alice E.", [alice])).toEqual([{ role: "Speaker 7", name: "Alice E.", person: "alice" }]);
  });
});

describe("withoutName", () => {
  it("returns a named entry to its label, clears a role-less person, and leaves never-named entries", async () => {
    const { withoutName } = await people();
    const untouched = { name: "Speaker 2" };
    expect(withoutName([untouched, { role: "Speaker 5", name: "Alice Example", person: "alice" }], "Speaker 5")).toEqual([untouched, { role: "Speaker 5", name: "Speaker 5" }]);
    expect(withoutName([bobLinked], "Bob Example")).toEqual([{ role: "Bob Example", name: "Bob Example" }]);
    expect(withoutName([untouched], "Speaker 2")[0]).toBe(untouched);
  });
});

describe("viewOf", () => {
  it("reads each speaker state from the record entry and the index", async () => {
    const { viewOf } = await people();
    const at = (label: string) => ({ rec: "wx49", label });
    const cases: [string, Speaker | undefined, string][] = [
      ["Speaker 3", undefined, "unnamed"],
      ["Speaker 3", { name: "Speaker 3" }, "unnamed"],
      ["Speaker 3", { role: "Speaker 3", name: "Bob Example" }, "resolving"],
      ["Bob Example", { role: "Bob Example", name: "Bob Example" }, "named"],
      ["Bob Example", undefined, "named"],
      ["Speaker 3", { role: "Speaker 3", name: "ALICE EXAMPLE" }, "linked"],
    ];
    for (const [label, entry, state] of cases) expect(viewOf(at(label), entry, [alice]).state).toBe(state);
    expect(viewOf(at("Bob Example"), bobLinked, [alice])).toMatchObject({ display: "Bob Example", person: { slug: "bob", unindexed: true } });
  });
});

describe("nameSpeaker", () => {
  const wx49 = { id: "wx49", state: "aligned", title: "Varnish curing", speakers: [bobLinked, { name: "Speaker 1" }], files: [{ role: "raw", file: "c5" }] };

  async function setup(rec: Record<string, unknown> = wx49) {
    const fake = fakeRegistry({ "rec.wx49": rec, "people.index": { people: [{ slug: "alice", name: "Alice Example (QA)", aliases: ["Alice E."] }] } });
    await loaded();
    return { fake, ...(await import("./people/store")) };
  }
  /** Lose the next `n` writes of rec.wx49 to another writer. */
  function lose(fake: ReturnType<typeof fakeRegistry>, n: number) {
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) => {
      if (init?.method === "PUT" && n > 0) {
        n--;
        fake.store.get("rec.wx49")!.version++;
      }
      return inner(url, init);
    });
  }

  it("writes speakers alone at the version read, linking through the index, after one lost race", async () => {
    const { fake, nameSpeaker } = await setup();
    lose(fake, 1);
    await nameSpeaker({ rec: "wx49", label: "Speaker 1" }, " alice e. ");
    expect(fake.writes).toEqual(["PUT rec.wx49 v=2"]);
    expect(fake.data("rec.wx49")).toEqual({ ...wx49, speakers: [bobLinked, { role: "Speaker 1", name: "alice e.", person: "alice" }] });
  });

  it("unnames with an empty name, and writes nothing when that changes nothing", async () => {
    const { fake, nameSpeaker, unnameSpeaker } = await setup();
    await nameSpeaker({ rec: "wx49", label: "Bob Example" }, "");
    expect(fake.data("rec.wx49")?.speakers).toEqual([{ role: "Bob Example", name: "Bob Example" }, { name: "Speaker 1" }]);
    await unnameSpeaker({ rec: "wx49", label: "Speaker 1" });
    expect(fake.writes).toEqual(["PUT rec.wx49 v=1"]);
  });

  it("stops after five lost races and says the recording changed", async () => {
    const { fake, nameSpeaker } = await setup();
    lose(fake, 5);
    await expect(nameSpeaker({ rec: "wx49", label: "Speaker 1" }, "Bob Example")).rejects.toThrow(/changed meanwhile/);
    expect(fake.writes).toEqual([]);
  });

  it("refuses a record deleted meanwhile", async () => {
    const { fake, nameSpeaker, DeletedMeanwhile } = await setup({ id: "wx49", state: "deleted" });
    await expect(nameSpeaker({ rec: "wx49", label: "Speaker 1" }, "Bob Example")).rejects.toBeInstanceOf(DeletedMeanwhile);
    expect(fake.writes).toEqual([]);
  });
});

const row = (id: string, started: string, speakers: Speaker[], state = "aligned") => ({ key: `rec.${id}`, version: 1, queued: false, agentLabels: [], rec: { id, state, started, speakers } });

describe("directory and waiting", () => {
  it("counts live meetings per person, and gathers pageless names and unnamed speakers", async () => {
    const { directory, waiting, viewOf, labelOf } = await people();
    const rows = [
      row("wx54", "2020-05-14T18:25:00Z", [bobLinked, { role: "Speaker 1", name: "Alice Example" }]),
      row("wx51", "2020-05-11T07:05:00Z", [{ name: "Speaker 1" }, { role: "Speaker 0", name: "alice e." }]),
      row("wx52", "2020-05-15T12:40:00Z", [bobLinked], "deleted"),
      row("wx53", "2020-05-12T21:10:00Z", [{ role: "Speaker 0", name: "Bob Example" }, { name: "Speaker 2" }]),
    ];
    const byRec = new Map(rows.map((r) => [r.rec.id, r.rec.speakers.map((s) => viewOf({ rec: r.rec.id, label: labelOf(s) }, s, [alice]))]));
    const d = { people: [alice], byRec };
    expect(directory(rows, d).map((e) => [e.person.slug, e.meetings.map((m) => m.row.rec.id), e.last?.toISOString()])).toEqual([
      ["alice", ["wx54", "wx51"], "2020-05-14T18:25:00.000Z"],
      ["bob", ["wx54"], "2020-05-14T18:25:00.000Z"],
    ]);
    const w = waiting(rows, d);
    expect(w.named.map((n) => [n.display, n.meetings.map((m) => m.rec.id), n.state])).toEqual([["Bob Example", ["wx53"], "resolving"]]);
    expect(w.unnamed.map((u) => [u.row.rec.id, u.views.map((v) => v.ref.label)])).toEqual([
      ["wx51", ["Speaker 1"]],
      ["wx53", ["Speaker 2"]],
    ]);
  });
});

describe("indexPeople", () => {
  const config = {
    wikis: {
      atelier: { page: "https://git.example/joinery/atelier/raw/{path}" },
      sketchbook: { page: "https://sketch.example/p/{path}" },
    },
  };
  // Alice has her primary page, the same page listed again, one whose path
  // needs escaping and an absolute one. Bob has a summary alone. The first
  // entry has no slug and is dropped.
  const idx = {
    people: [
      { name: "No Slug" },
      { slug: "bob", name: "Bob Example", wiki: "atelier", page: "bob.md", summary: "  Sorts the veneer offcuts.\n" },
      {
        slug: "alice",
        name: "Alice Example",
        role: "Cooper",
        org: "Barrelworks",
        wiki: "atelier",
        page: "people/alice.md",
        pages: [
          { wiki: "sketchbook", page: "drawers/alice#row.md", slug: "alice-row" },
          { wiki: "atelier", page: "people/alice.md", slug: "alice" },
          { wiki: "sketchbook", page: "https://wiki.example.net/alice-2", slug: "alice-2" },
        ],
      },
    ],
  } as unknown as PeopleIndexRecord;

  it("puts the primary page first, drops its repeat, and addresses each page through the page config", async () => {
    fakeRegistry({ config });
    await loaded();
    const { indexPeople, pageURL, roleLine, personBySlug } = await import("./people/store");
    const list = indexPeople(idx);
    expect(list.map((p) => p.slug)).toEqual(["bob", "alice"]);
    const [bob, a] = list;
    expect(a.pages.map((p) => p.slug)).toEqual(["alice", "alice-row", "alice-2"]);
    expect(a.pages.map(pageURL)).toEqual(["https://git.example/joinery/atelier/raw/people/alice.md", "https://sketch.example/p/drawers/alice%23row.md", "https://wiki.example.net/alice-2"]);
    expect(bob.wiki_url).toBe("https://git.example/joinery/atelier/raw/bob.md");
    expect(list.map(roleLine)).toEqual(["Sorts the veneer offcuts.", "Cooper · Barrelworks"]);
    expect(personBySlug("alice-row", list)?.slug).toBe("alice");
  });

  it("gives a role without an org as the role line, and no address in a wiki the config does not know", async () => {
    fakeRegistry({ config });
    await loaded();
    const { indexPeople, roleLine } = await import("./people/store");
    const [bob] = indexPeople({ people: [{ slug: "bob", name: "Bob Example", wiki: "offsite", page: "bob.md", role: "Glazier" }] } as unknown as PeopleIndexRecord);
    expect([bob.wiki_url, roleLine(bob)]).toEqual(["", "Glazier"]);
  });

  it("addresses no page without a page config", async () => {
    const { indexPeople } = await people();
    expect(indexPeople(idx).map((p) => p.wiki_url)).toEqual(["", ""]);
  });
});
