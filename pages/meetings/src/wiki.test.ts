import { describe, expect, it } from "vitest";
import { wikiLink } from "./wiki";

const cfg = {
  wikis: {
    "team-wiki": {
      page: "https://git.example/team/team-wiki/src/branch/main/{path}",
      commit: "https://git.example/team/team-wiki/src/commit/{commit}/{path}",
    },
  },
};

describe("wikiLink", () => {
  it("fills the page template, each path segment encoded", () => {
    expect(wikiLink(cfg, "team-wiki", "meetings/2020 01/a#b.md")).toBe("https://git.example/team/team-wiki/src/branch/main/meetings/2020%2001/a%23b.md");
  });
  it("links the commit when given and templated", () => {
    expect(wikiLink(cfg, "team-wiki", "meetings/a.md", "abc123")).toBe("https://git.example/team/team-wiki/src/commit/abc123/meetings/a.md");
  });
  it("keeps a full URL, and links nothing for an unknown wiki or no config", () => {
    expect(wikiLink(null, undefined, "https://pages.example/x")).toBe("https://pages.example/x");
    expect(wikiLink(cfg, "other-wiki", "a.md")).toBe("");
    expect(wikiLink(null, "team-wiki", "a.md")).toBe("");
  });
});
