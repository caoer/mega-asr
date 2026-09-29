import { afterEach, describe, expect, it, vi } from "vitest";
import { fakeRegistry, loaded } from "./fake-registry";

afterEach(() => vi.unstubAllGlobals());

// A weather-station log: two meeting types and a person on the closed list,
// the rest open topics. The record lists its type last.
const closed = {
  closed: [
    { name: "巡检", kind: "type" },
    { name: "补测", kind: "type" },
    { name: "人:Alice Example", kind: "person" },
  ],
};
const wx41 = {
  id: "wx41",
  state: "ingested",
  started: "2020-02-17T06:40:00Z",
  duration_s: 1260,
  title: "Anemometer swap",
  labels: ["风速计", "巡检"],
  files: [{ role: "raw", file: "g2" }],
};

async function setup(rec: Record<string, unknown> = wx41) {
  const fake = fakeRegistry({ labels: closed, "rec.wx41": rec });
  const store = await loaded();
  const model = await import("./model");
  return { fake, store, model, stored: () => fake.store.get("rec.wx41")! };
}

describe("setLabels", () => {
  it("writes nothing when the change leaves the labels as they are", async () => {
    const { fake, store, model } = await setup();
    await store.setLabels("wx41", model.removeLabel("露点"));
    expect(fake.writes).toEqual([]);
  });

  it("puts a new type in place of the old one, keeping the schema and every other field", async () => {
    const { fake, store, model, stored } = await setup();
    await store.setLabels("wx41", model.addLabel("补测"));
    expect(fake.writes).toEqual(["PUT rec.wx41 v=1"]);
    expect(stored().value).toEqual({ schema: "meeting@1", data: { ...wx41, labels: ["风速计", "补测"] } });
  });

  it("trims and de-duplicates what the change returns", async () => {
    const { store, fake } = await setup();
    await store.setLabels("wx41", (cur) => [...cur, " 雨量筒 ", "雨量筒", ""]);
    expect(fake.data("rec.wx41")?.labels).toEqual(["风速计", "巡检", "雨量筒"]);
  });

  it("applies the change again on top of a writer that got in first", async () => {
    const { fake, store, model, stored } = await setup();
    let seen = 0;
    await store.setLabels("wx41", (cur) => {
      if (seen++ === 0) {
        const s = stored();
        s.version = 2;
        s.value = { ...s.value, data: { ...s.value.data, labels: ["风速计", "巡检", "百叶箱"] } };
      }
      return model.addLabel("露点")(cur);
    });
    expect(seen).toBe(2);
    expect(fake.writes).toEqual(["PUT rec.wx41 v=2"]);
    expect(fake.data("rec.wx41")?.labels).toEqual(["风速计", "巡检", "百叶箱", "露点"]);
  });

  it("stops after LABEL_TRIES lost races and says the recording changed", async () => {
    const { fake, store, model, stored } = await setup();
    let tries = 0;
    const lost = store.setLabels("wx41", (cur) => {
      tries++;
      stored().version++;
      return model.addLabel("人:Alice Example")(cur);
    });
    await expect(lost).rejects.toThrow(/changed meanwhile/);
    expect(tries).toBe(store.LABEL_TRIES);
    expect(fake.writes).toEqual([]);
  });

  it("refuses a record deleted meanwhile before any write", async () => {
    const { fake, store, model } = await setup({ id: "wx41", state: "deleted" });
    await expect(store.setLabels("wx41", model.addLabel("风速计"))).rejects.toThrow(/deleted meanwhile/);
    expect(fake.writes).toEqual([]);
  });

  it("passes any refusal other than a conflict straight through", async () => {
    const { fake, store, model } = await setup();
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (url: string, init?: RequestInit) =>
      init?.method === "PUT" ? Promise.resolve(new Response(JSON.stringify({ error: "not yours", code: "not_creator" }), { status: 403 })) : inner(url, init),
    );
    let tries = 0;
    const err = await store
      .setLabels("wx41", (cur) => {
        tries++;
        return model.addLabel("雨量计")(cur);
      })
      .catch((e) => e);
    expect(err).toMatchObject({ status: 403, code: "not_creator" });
    expect(tries).toBe(1);
    expect(fake.writes).toEqual([]);
  });
});
