import { afterEach, describe, expect, it, vi } from "vitest";
import { fakeRegistry, loaded } from "./fake-registry";
import { conflictText, liveClaim, TOMBSTONE_KEEPS, tombstone } from "./actions";
import type { Rec } from "./types";

afterEach(() => vi.unstubAllGlobals());

// Every field megameet's keep list names except `owner`, among fields a
// tombstone drops; three audio files and no Feishu link.
const wx61 = {
  state: "ingested",
  files: [
    { role: "mic", file: "n1" },
    { role: "remote", file: "n2" },
    { role: "beam", file: "n3" },
  ],
  wiki: { page: "notes/wx61.md", commit: "d15ea5e" },
  stopped: "2020-06-09T11:02:10Z",
  labels: ["百叶箱", "露点", "补测"],
  host: "bench-pc",
  summary: "Two blades honed.",
  id: "wx61",
  started: "2020-06-09T10:14:55Z",
  title: "Clamp walkthrough",
  source: "local",
  duration_s: 2835,
};

describe("tombstone", () => {
  it("keeps the listed fields the record has, and nothing it lacks", () => {
    const t = tombstone(wx61 as Rec, new Date("2020-06-11T00:00:00.001Z"));
    const kept = TOMBSTONE_KEEPS.filter((k) => k in t);
    expect(TOMBSTONE_KEEPS.filter((k) => !kept.includes(k))).toEqual(["owner"]);
    expect(Object.keys(t).filter((k) => !(TOMBSTONE_KEEPS as readonly string[]).includes(k)).sort()).toEqual(["deleted_at", "deleted_by", "state", "updated"]);
    for (const k of kept) expect(t[k]).toEqual(wx61[k as keyof typeof wx61]);
    expect(t).toMatchObject({ state: "deleted", deleted_at: "2020-06-11T00:00:00Z", deleted_by: "meetings page" });
  });

  it("reduces a Feishu link to its token, and drops one without a token", () => {
    const feishu = (f: Record<string, string>) => tombstone({ id: "wx62", state: "failed", feishu: f } as Rec).feishu;
    expect(feishu({ token: "obcn5t3", url: "https://tenant.example/minutes/obcn5t3", transcript_file: "n4" })).toEqual({ token: "obcn5t3" });
    expect(feishu({ url: "https://tenant.example/minutes/x" })).toBeUndefined();
  });
});

describe("liveClaim and conflictText", () => {
  const leased = (expires: string) => ({ id: "wx63", state: "processing", claim: { by: "", expires } }) as Rec;
  it("honours a lease only until it expires", () => {
    const now = Date.parse("2020-06-09T12:00:00Z");
    expect(liveClaim(leased("2020-06-09T12:00:01Z"), now)).toEqual({ by: "", expires: "2020-06-09T12:00:01Z" });
    expect(liveClaim(leased("2020-06-09T12:00:00Z"), now)).toBeNull();
    expect(liveClaim(leased("later"), now)).toBeNull();
    expect(liveClaim(undefined, now)).toBeNull();
  });
  it("says why a write did not land", () => {
    expect(conflictText(leased("2999-01-01T00:00:00Z"))).toMatch(/^In progress on another host/);
    expect(conflictText(null)).toMatch(/^The recording changed since this page read it\. /);
    expect(conflictText({ id: "wx63", state: "deleted" })).toMatch(/deleted meanwhile/);
    expect(conflictText({ id: "wx63", state: "aligned" })).toMatch(/\(now aligned\)/);
  });
});

describe("deleteRecording", () => {
  it("tombstones the record and removes every file it names or that names it, with no queue key to drop", async () => {
    const fake = fakeRegistry({
      "rec.wx61": wx61,
      "f.n2": { meta: { rec: "wx61" } },
      "f.n3": { meta: { rec: "wx61" } },
      "f.p7": { meta: {} },
      "f.p8": { meta: { rec: "wx61" } },
      "f.p9": { meta: { rec: "wx61" } },
    });
    await loaded();
    const { deleteRecording } = await import("./actions");
    await deleteRecording("wx61");
    expect(fake.data("rec.wx61")).toMatchObject({ id: "wx61", state: "deleted", host: "bench-pc" });
    expect(fake.data("rec.wx61")).not.toHaveProperty("labels");
    // n1 is not stored: its 404 is not an error, and neither is the absent q.wx61.
    expect(fake.writes).toEqual(["PUT rec.wx61 v=1", "DELETE f/n2", "DELETE f/n3", "DELETE f/p8", "DELETE f/p9"]);
    expect([...fake.store.keys()]).toEqual(["rec.wx61", "f.p7"]);
  });

  it("writes nothing under a live lease whose holder is unnamed", async () => {
    const fake = fakeRegistry({ "rec.wx61": { ...wx61, claim: { by: "", expires: "2999-06-01T00:00:00Z" } } });
    await loaded();
    const { deleteRecording } = await import("./actions");
    await expect(deleteRecording("wx61")).rejects.toThrow(/^In progress on another host/);
    expect(fake.writes).toEqual([]);
  });

  it("writes nothing when the record is not the version the page listed", async () => {
    const fake = fakeRegistry({ "rec.wx64": { id: "wx64", state: "aligned" } });
    await loaded();
    fake.store.get("rec.wx64")!.version = 3;
    const { deleteRecording } = await import("./actions");
    await expect(deleteRecording("wx64")).rejects.toThrow(/changed since this page read it \(now aligned\)/);
    expect(fake.writes).toEqual([]);
  });

  it("writes nothing when the record is gone since the page listed it", async () => {
    const fake = fakeRegistry({ "rec.wx61": wx61 });
    await loaded();
    fake.store.delete("rec.wx61");
    const { deleteRecording } = await import("./actions");
    await expect(deleteRecording("wx61")).rejects.toThrow(/deleted meanwhile/);
    expect(fake.writes).toEqual([]);
  });
});

describe("queueAction", () => {
  it("sets the action at the listed version and then creates q.<id>", async () => {
    const fake = fakeRegistry({ "rec.wx65": { id: "wx65", state: "aligned", title: "Oiling the walnut" } });
    await loaded();
    const { queueAction } = await import("./actions");
    await queueAction("wx65", "realign");
    expect(fake.writes).toEqual(["PUT rec.wx65 v=1", "PUT q.wx65 v=0"]);
    expect(fake.data("rec.wx65")).toMatchObject({ state: "aligned", action: "realign", title: "Oiling the walnut" });
    expect(fake.data("q.wx65")).toEqual({ state: "aligned" });
  });

  it("refuses an action the record's state does not allow", async () => {
    const fake = fakeRegistry({ "rec.wx65": { id: "wx65", state: "uploaded" } });
    await loaded();
    const { queueAction } = await import("./actions");
    await expect(queueAction("wx65", "retry")).rejects.toThrow(/changed since this page read it \(now uploaded\)/);
    expect(fake.writes).toEqual([]);
  });
});
