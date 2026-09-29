// A wiki page's web address, from the page's `config` record.
import type { PageConfig } from "./types";

/**
 * `page` (repo-relative, or already a full URL) in wiki `wiki`, at `commit`
 * when given and the wiki has a commit template; "" when the config does not
 * know the wiki.
 */
export function wikiLink(cfg: PageConfig | null, wiki: string | undefined, page: string, commit?: string): string {
  if (/^https?:\/\//.test(page)) return page;
  const t = wiki ? cfg?.wikis?.[wiki] : undefined;
  const tpl = commit && t?.commit ? t.commit : t?.page;
  if (typeof tpl !== "string" || !tpl.includes("{path}")) return "";
  const path = page.split("/").map(encodeURIComponent).join("/");
  return tpl.replace("{commit}", encodeURIComponent(commit ?? "")).replace("{path}", path);
}
