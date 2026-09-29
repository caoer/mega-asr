import { afterEach, describe, expect, it, vi } from "vitest";
import { fakeRegistry, loaded } from "./fake-registry";

afterEach(() => vi.unstubAllGlobals());

const at = new Date(2020, 1, 29, 16, 5, 42);
const meta = (over = {}) => ({ file: new File(["fourteen bytes"], "clip.m4a", { type: "audio/mp4" }), title: "memo", speakers: ["Pip", "青柠"], recordedAt: at, device: "Field kit", duration: 47.06, ...over });

async function upload() {
  const s = await loaded();
  const u = await import("./uploadFlow");
  return { s, u };
}

describe("startUpload", () => {
  it("moves the id one second on when the first is taken", async () => {
    const fake = fakeRegistry({ "rec.20200229-160542-phone-field-kit": { id: "taken", state: "aligned" } });
    const { u } = await upload();
    const { id, done } = await u.startUpload(meta());
    await done;
    expect(id).toBe("20200229-160543-phone-field-kit");
    expect(fake.writes[0]).toBe("PUT rec.20200229-160543-phone-field-kit v=0");
  });

  it("writes the record uploading before the bytes and uploaded after, then queues it", async () => {
    const fake = fakeRegistry();
    const { s, u } = await upload();
    const { id, done } = await u.startUpload(meta());
    const key = `rec.${id}`;
    expect(fake.data(key)).toMatchObject({ state: "uploading", files: [], source: "phone", host: "field-kit", title: "memo", speakers: [{ name: "Pip" }, { name: "青柠" }], duration_s: 47.1 });
    expect(s.getState().progress[id]).toBe(0);
    await done;
    expect(fake.writes).toEqual([`PUT ${key} v=0`, "POST f", `PUT ${key} v=1`, `PUT q.${id} v=0`]);
    const rec = fake.data(key) as { state: string; files: { role: string; file: string; sha256: string }[] };
    expect(rec.state).toBe("uploaded");
    expect(rec.files).toMatchObject([{ role: "media", file: "file1", bytes: 14, codec: "m4a", verified: false }]);
    expect(fake.data("f.file1")).toEqual({ meta: { rec: id, role: "media" } });
    expect(s.getState().progress[id]).toBeUndefined();
  });

  it("leaves the record uploading when the POST fails, and says so", async () => {
    const fake = fakeRegistry();
    fake.failPost = true;
    const { s, u } = await upload();
    const { id, done } = await u.startUpload(meta());
    await expect(done).rejects.toThrow(/Upload failed: the upload was interrupted\. The recording stays listed as uploading/);
    expect(fake.data(`rec.${id}`)).toMatchObject({ state: "uploading" });
    expect(fake.store.has(`q.${id}`)).toBe(false);
    expect(s.getState().progress[id]).toBeUndefined();
  });
});
